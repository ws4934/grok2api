package gateway

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestGatewayBillsCustomPriceAndKeepsOfficialGrokCost(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "gateway-pricing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	const customProvider account.Provider = "pricedgpt"
	const customModel, grokModel = "gpt-priced", "grok-4.3"
	for _, target := range []struct {
		provider account.Provider
		model    string
	}{{customProvider, customModel}, {account.ProviderConsole, grokModel}} {
		credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: target.provider, AuthType: account.AuthTypeSSO, Name: string(target.provider), SourceKey: string(target.provider),
			EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := modelRepo.UpsertDiscovered(ctx, target.provider, []string{target.model}); err != nil {
			t.Fatal(err)
		}
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{target.model}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "priced", Prefix: "priced", SecretHash: strings.Repeat("d", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4, BillingLimitUSDTicks: 100_000_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(statelessConsoleAdapter{}, &ssoFailureAdapter{providerValue: customProvider})
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 1)
	prices, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(account.Provider) bool { return true }, registry.PricingModel)
	if err != nil {
		t.Fatal(err)
	}
	price, err := prices.Put(ctx, pricingapp.Input{Provider: string(customProvider), PricingModel: customModel, Operation: "text", Rates: map[pricingdomain.Component]string{
		pricingdomain.InputToken: "2", pricingdomain.CachedInputToken: "0.5", pricingdomain.OutputToken: "8",
	}})
	if err != nil {
		t.Fatal(err)
	}
	service.SetPricing(prices)

	usage := Usage{Reported: true, InputTokens: 1000, CachedInputTokens: 200, OutputTokens: 300, TotalTokens: 1300}
	billed := int64(0)
	for _, tc := range []struct {
		model     string
		wantModel string
		wantCost  int64
		priceID   uint64
		version   string
	}{
		{customModel, customModel, 800*20_000 + 200*5_000 + 300*80_000, price.ID, audit.CustomPricingVersion},
		{grokModel, "grok-4.3", 0, 0, audit.OfficialPricingAsOf},
	} {
		if tc.priceID == 0 {
			official, ok := audit.EstimateOfficialCost(grokModel, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, 0)
			if !ok {
				t.Fatal("official grok price unavailable")
			}
			tc.wantCost = official.CostInUSDTicks
		}
		body := []byte(`{"model":"` + tc.model + `","input":"hello"}`)
		result, err := service.CreateResponse(ctx, Input{RequestID: "req-" + tc.model, ClientKey: key, PublicModel: tc.model, Body: body})
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		reserved, err := keyRepo.Get(ctx, key.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantReservation, _ := audit.EstimateOfficialTextReservation(grokModel, body)
		if tc.priceID != 0 {
			input, output := audit.EstimateReservationTokens(body)
			wantReservation.CostInUSDTicks = input*20_000 + output*80_000
		}
		if reserved.ReservedUsageUSDTicks != wantReservation.CostInUSDTicks {
			t.Fatalf("%s reservation = %d, want %d", tc.model, reserved.ReservedUsageUSDTicks, wantReservation.CostInUSDTicks)
		}
		_, _ = io.ReadAll(result.Body)
		result.Finalize(usage, "", "")
		_ = result.Body.Close()

		billed += tc.wantCost
		settled, err := keyRepo.Get(ctx, key.ID)
		if err != nil {
			t.Fatal(err)
		}
		if settled.BilledUsageUSDTicks != billed || settled.ReservedUsageUSDTicks != 0 {
			t.Fatalf("%s billing = billed %d reserved %d, want billed %d", tc.model, settled.BilledUsageUSDTicks, settled.ReservedUsageUSDTicks, billed)
		}
		records, _, err := auditRepo.List(ctx, 0, 1)
		if err != nil || len(records) != 1 {
			t.Fatalf("audits = %#v, %v", records, err)
		}
		record := records[0]
		if record.EstimatedCostInUSDTicks != tc.wantCost || record.PricingModel != tc.wantModel || record.PriceID != tc.priceID || record.PricingVersion != tc.version {
			t.Fatalf("%s audit = cost %d model %q price %d version %q", tc.model, record.EstimatedCostInUSDTicks, record.PricingModel, record.PriceID, record.PricingVersion)
		}
	}
}
