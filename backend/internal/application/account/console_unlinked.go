package account

import (
	"context"
	"errors"
	"fmt"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// ssoRejectedReason keeps the generic SSO rejection text except for Console
// accounts proven unlinked from Cursor, whose marker drives optional deletion.
func ssoRejectedReason(value accountdomain.Credential, err error) string {
	if value.Provider == accountdomain.ProviderConsole && errors.Is(err, provider.ErrConsoleUnlinked) {
		return accountdomain.LastErrorConsoleUnlinked
	}
	return fmt.Sprintf("%s SSO credential rejected", value.Provider)
}

func isConsoleUnlinkedAccount(value accountdomain.Credential) bool {
	return value.Provider == accountdomain.ProviderConsole && value.AuthStatus == accountdomain.AuthStatusReauthRequired && value.LastError == accountdomain.LastErrorConsoleUnlinked
}

// UpdateAutoDeleteConsoleUnlinked hot-updates deletion of unlinked Console
// accounts. Turning it on also sweeps accounts already marked earlier.
func (s *Service) UpdateAutoDeleteConsoleUnlinked(enabled bool) {
	if s.autoDeleteConsoleUnlinked.Swap(enabled) == enabled || !enabled {
		return
	}
	go s.sweepConsoleUnlinkedAccounts(context.Background())
}

// deleteConsoleUnlinkedAccount removes only the Console row; linked Web/Build
// accounts stay. A pending video job makes the delete fail closed and the
// account simply remains reauthRequired.
func (s *Service) deleteConsoleUnlinkedAccount(ctx context.Context, id uint64) {
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	if err := s.Delete(deleteCtx, id); err != nil && !errors.Is(err, ErrNotFound) {
		s.logger.Warn("console_unlinked_account_delete_failed", "account_id", id, "error", err)
		return
	}
	s.logger.Info("console_unlinked_account_deleted", "account_id", id)
}

func (s *Service) sweepConsoleUnlinkedAccounts(ctx context.Context) {
	ids, err := s.accounts.ListReauthAccountIDsByLastError(ctx, accountdomain.ProviderConsole, accountdomain.LastErrorConsoleUnlinked)
	if err != nil {
		s.logger.Warn("console_unlinked_account_sweep_failed", "error", err)
		return
	}
	for _, id := range ids {
		if !s.autoDeleteConsoleUnlinked.Load() || ctx.Err() != nil {
			return
		}
		s.deleteConsoleUnlinkedAccount(ctx, id)
	}
}
