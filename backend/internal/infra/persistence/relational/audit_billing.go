package relational

import (
	"errors"
	"math"
	"sort"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func settleInsertedAudits(tx *gorm.DB, inserted []preparedAudit) error {
	if len(inserted) == 0 {
		return nil
	}
	keySet := make(map[uint64]struct{}, len(inserted))
	for _, value := range inserted {
		keySet[value.row.ClientKeyID] = struct{}{}
	}
	keyIDs := make([]uint64, 0, len(keySet))
	for keyID := range keySet {
		keyIDs = append(keyIDs, keyID)
	}
	sort.Slice(keyIDs, func(i, j int) bool { return keyIDs[i] < keyIDs[j] })
	// Lock every referenced key before reading reservations so zero-cost settlements cannot race reservation creation.
	missingKeys := make(map[uint64]struct{})
	for _, keyID := range keyIDs {
		if err := lockClientKey(tx, keyID); err != nil {
			if !errors.Is(err, repository.ErrNotFound) {
				return err
			}
			missingKeys[keyID] = struct{}{}
		}
	}

	eventIDs := make([]string, 0, len(inserted))
	for _, value := range inserted {
		eventIDs = append(eventIDs, value.row.EventID)
	}
	var reservations []billingReservationModel
	if err := tx.Where("event_id IN ?", eventIDs).Find(&reservations).Error; err != nil {
		return err
	}
	reservationByEvent := make(map[string]billingReservationModel, len(reservations))
	for _, reservation := range reservations {
		if _, missing := missingKeys[reservation.ClientKeyID]; missing {
			return repository.ErrNotFound
		}
		reservationByEvent[reservation.EventID] = reservation
	}

	sort.Slice(inserted, func(i, j int) bool {
		if inserted[i].row.ClientKeyID == inserted[j].row.ClientKeyID {
			return inserted[i].row.EventID < inserted[j].row.EventID
		}
		return inserted[i].row.ClientKeyID < inserted[j].row.ClientKeyID
	})
	settledEventIDs := make([]string, 0, len(reservations))
	for _, value := range inserted {
		reservation, hasReservation := reservationByEvent[value.row.EventID]
		settled, err := applyInsertedAuditBilling(tx, value.row, reservation, hasReservation)
		if err != nil {
			return err
		}
		if settled {
			settledEventIDs = append(settledEventIDs, value.row.EventID)
		}
	}
	return deleteBillingReservations(tx, settledEventIDs)
}

func applyInsertedAuditBilling(tx *gorm.DB, row requestAuditModel, reservation billingReservationModel, hasReservation bool) (bool, error) {
	amount := auditBillingAmount(row)
	if hasReservation {
		if err := settleReservedBilling(tx, reservation, amount); err != nil {
			return false, err
		}
		return true, nil
	}
	if amount <= 0 {
		return false, nil
	}
	result := tx.Model(&clientKeyModel{}).Where("id = ?", row.ClientKeyID).UpdateColumn(
		"billed_usage_usd_ticks",
		gorm.Expr("CASE WHEN billed_usage_usd_ticks > ? THEN ? ELSE billed_usage_usd_ticks + ? END", math.MaxInt64-amount, int64(math.MaxInt64), amount),
	)
	if result.Error != nil {
		return false, result.Error
	}
	return false, nil
}

func deleteBillingReservations(tx *gorm.DB, eventIDs []string) error {
	if len(eventIDs) == 0 {
		return nil
	}
	result := tx.Where("event_id IN ?", eventIDs).Delete(&billingReservationModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != int64(len(eventIDs)) {
		return repository.ErrConflict
	}
	return nil
}

// auditBillingAmount bills an administrator price (PriceID != 0) at its own
// estimate; otherwise the upstream-reported cost wins over the official estimate.
func auditBillingAmount(row requestAuditModel) int64 {
	if row.PriceID == 0 && row.CostInUSDTicks > 0 {
		return row.CostInUSDTicks
	}
	return row.EstimatedCostInUSDTicks
}

func settleReservedBilling(tx *gorm.DB, reservation billingReservationModel, amount int64) error {
	if amount < 0 {
		amount = 0
	}
	result := tx.Model(&clientKeyModel{}).Where("id = ?", reservation.ClientKeyID).Updates(map[string]any{
		"reserved_usage_usd_ticks": gorm.Expr("CASE WHEN reserved_usage_usd_ticks <= ? THEN 0 ELSE reserved_usage_usd_ticks - ? END", reservation.Amount, reservation.Amount),
		"billed_usage_usd_ticks":   gorm.Expr("CASE WHEN billed_usage_usd_ticks > ? THEN ? ELSE billed_usage_usd_ticks + ? END", math.MaxInt64-amount, int64(math.MaxInt64), amount),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}
