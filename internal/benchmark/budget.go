package benchmark

import "fmt"

// EstimateMatrixCost is the dry-run cost estimate (plan §3.7 step 1): the cell
// count × expected turns-per-cell × historical per-turn USD for the model
// class. Pure arithmetic — the runner supplies usdPerTurn from the store's
// historical per-turn cost and prints this before requiring confirmation.
func EstimateMatrixCost(cells, turnsPerCell int, usdPerTurn float64) float64 {
	if cells <= 0 || turnsPerCell <= 0 || usdPerTurn <= 0 {
		return 0
	}
	return float64(cells) * float64(turnsPerCell) * usdPerTurn
}

// CellCapExceeded reports whether an in-flight attempt has breached any
// per-attempt cap (USD / turns / wall-seconds). Returns the machine-readable
// reason for the ledger's error_class. A zero cap means "no cap for this
// dimension".
func (b Budget) CellCapExceeded(spendUSD float64, turns, wallSec int) (bool, string) {
	if b.MaxCellUSD > 0 && spendUSD > b.MaxCellUSD {
		return true, fmt.Sprintf("cell_usd_cap: $%.4f > $%.4f", spendUSD, b.MaxCellUSD)
	}
	if b.MaxTurnsPerCell > 0 && turns > b.MaxTurnsPerCell {
		return true, fmt.Sprintf("cell_turns_cap: %d > %d", turns, b.MaxTurnsPerCell)
	}
	if b.MaxWallSecCell > 0 && wallSec > b.MaxWallSecCell {
		return true, fmt.Sprintf("cell_wall_cap: %ds > %ds", wallSec, b.MaxWallSecCell)
	}
	return false, ""
}

// RunCapExceeded reports whether the accumulated run spend has breached the
// per-run cap. On breach the runner marks remaining cells budget_stop and
// finalizes cleanly (plan §3.7 step 3).
func (b Budget) RunCapExceeded(totalSpendUSD float64) bool {
	return b.MaxTotalUSD > 0 && totalSpendUSD >= b.MaxTotalUSD
}

// JudgeCapExceeded reports whether accumulated judge spend has breached the
// separate judge budget (plan §3.7 step 4) — accounted apart from cell spend so
// the two can't cross-subsidize.
func (b Budget) JudgeCapExceeded(judgeSpendUSD float64) bool {
	return b.JudgeBudgetUSD > 0 && judgeSpendUSD >= b.JudgeBudgetUSD
}
