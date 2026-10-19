package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type ResourceInput struct {
	ClientKey  clientkey.Key
	ResponseID string
	RawQuery   string
}

func (s *Service) GetResponse(ctx context.Context, input ResourceInput) (*Result, error) {
	return s.forwardOwnedResponse(ctx, input, http.MethodGet)
}

func (s *Service) DeleteResponse(ctx context.Context, input ResourceInput) (*Result, error) {
	return s.forwardOwnedResponse(ctx, input, http.MethodDelete)
}

func (s *Service) forwardOwnedResponse(ctx context.Context, input ResourceInput, method string) (*Result, error) {
	ownership, err := s.responses.Get(ctx, input.ResponseID, input.ClientKey.ID, time.Now().UTC())
	if err != nil {
		return nil, ErrResponseNotFound
	}
	if !s.providers.SupportsStoredResponses(ownership.Provider) {
		_ = s.responses.Delete(ctx, input.ResponseID, input.ClientKey.ID)
		return nil, ErrResponseNotFound
	}
	accountScope := input.ClientKey.AccountScope()
	if !accountScope.AllowsProvider(ownership.Provider) {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	adapter, ok := s.providers.Responses(ownership.Provider)
	if !ok {
		return nil, ErrResponseAccountUnavailable
	}
	operation := "response_get"
	if method == http.MethodDelete {
		operation = "response_delete"
	}
	physicalCallCtx := infraegress.WithPhysicalCallTrace(ctx, string(ownership.Provider), operation)
	lease, err := s.selector.AcquirePinnedForKey(ctx, ownership.Provider, ownership.AccountID, 0, "", "", false, accountScope)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, err)
	}
	credential, err := s.accounts.EnsureCredential(ctx, lease.Credential, false)
	if err != nil {
		lease.Release()
		return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, err)
	}
	path := "/responses/" + url.PathEscape(input.ResponseID)
	if input.RawQuery != "" {
		path += "?" + input.RawQuery
	}
	response, err := adapter.ForwardResponse(physicalCallCtx, provider.ResponseResourceRequest{Credential: credential, Method: method, Path: path})
	if err != nil {
		if isSSOCredentialRejected(err, credential) {
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
		}
		lease.Release()
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		if credential.AuthType == accountdomain.AuthTypeSSO {
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
			lease.Release()
			return nil, ErrResponseAccountUnavailable
		}
		if s.markPermanentlyUnrefreshableCredentialRejected(ctx, credential) {
			lease.Release()
			return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, accountapp.ErrCredentialRefreshPermanent)
		}
		refreshed, refreshErr := s.accounts.RenewRejectedCredential(ctx, credential)
		if refreshErr != nil {
			if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
				s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
			}
			s.selector.MarkRefreshDeferred(ctx, credential, refreshErr)
			lease.Release()
			return nil, refreshErr
		}
		response, err = adapter.ForwardResponse(physicalCallCtx, provider.ResponseResourceRequest{Credential: refreshed, Method: method, Path: path})
		credential = refreshed
		if err != nil {
			lease.Release()
			return nil, err
		}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		s.selector.markSuccess(ctx, credential, false)
		if method == http.MethodDelete {
			_ = s.responses.Delete(ctx, input.ResponseID, input.ClientKey.ID)
		}
	} else if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		_ = s.responses.Delete(ctx, input.ResponseID, input.ClientKey.ID)
	}
	var once sync.Once
	release := func() { once.Do(lease.Release) }
	finalize := func(Usage, string, string) { release() }
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: release}, Finalize: finalize}, nil
}
