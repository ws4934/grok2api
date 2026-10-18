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
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// A Console request under an administrator markup is settled at the custom
// price, not at the xAI-reported cost_in_usd_ticks.
func TestCustomPriceSettlesOverUpstreamReportedCost(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "custom-price.db"))
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
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: "console", SourceKey: "console",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderConsole, []string{"grok-4.3"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.3"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{Name: "custom-price", Prefix: "custom", SecretHash: strings.Repeat("d", 64), EncryptedSecret: "e",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4, BillingLimitUSDTicks: 100_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(statelessConsoleAdapter{})
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 1)
	prices, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(account.Provider) bool { return true }, registry.PricingModel)
	if err != nil {
		t.Fatal(err)
	}
	// $100/M input and output: a markup over the official Console rate.
	if _, err := prices.Put(ctx, pricingapp.Input{Provider: string(account.ProviderConsole), PricingModel: "grok-4.3", Operation: "text", Rates: map[pricingdomain.Component]string{
		pricingdomain.InputToken: "100", pricingdomain.OutputToken: "100",
	}}); err != nil {
		t.Fatal(err)
	}
	service.SetPricing(prices)

	result, err := service.CreateResponse(ctx, Input{RequestID: "custom-price", ClientKey: key, PublicModel: "grok-4.3", Body: []byte(`{"model":"grok-4.3","input":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{Reported: true, InputTokens: 1000, OutputTokens: 1000, TotalTokens: 2000, CostInUSDTicks: 5}, "", "")
	_ = result.Body.Close()

	const custom = int64(2000 * 1_000_000) // 2000 tokens at 1e6 ticks per token
	settled, err := keyRepo.Get(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	records, _, err := auditRepo.List(ctx, 0, 1)
	if err != nil || len(records) != 1 {
		t.Fatalf("audits = %#v, %v", records, err)
	}
	if records[0].PriceID == 0 || records[0].EstimatedCostInUSDTicks != custom || records[0].CostInUSDTicks != 5 {
		t.Fatalf("audit = price %d estimate %d cost %d", records[0].PriceID, records[0].EstimatedCostInUSDTicks, records[0].CostInUSDTicks)
	}
	if settled.BilledUsageUSDTicks != custom || settled.ReservedUsageUSDTicks != 0 {
		t.Fatalf("billed %d reserved %d, want billed %d", settled.BilledUsageUSDTicks, settled.ReservedUsageUSDTicks, custom)
	}
}
