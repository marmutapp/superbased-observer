package pricing

// priceWithTax and bulkPriceWithTax both inline the SAME "add tax, then
// round to the nearest cent" arithmetic. That duplicated rounding
// computation is the refactor target.
//
// REFACTOR TASK: extract the duplicated tax-and-round computation into
// ONE helper and call it from both functions, WITHOUT changing observable
// behavior (the tests must stay green) and WITHOUT editing
// pricing_test.go or deleting either function. After a correct refactor
// the rounding divisor appears in a single place, not two.

func priceWithTax(cents int) int {
	return (cents*108 + 50) / 100
}

func bulkPriceWithTax(cents, qty int) int {
	return ((cents*qty)*108 + 50) / 100
}
