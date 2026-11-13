//go:build qualify

package qualify

import "testing"

// The budget usage and the budget are required numbers: a missing field, a JSON null, a string or a negative number is
// an error and never reads as 0. No hub is needed.
func TestParseBudgetUsage(t *testing.T) {
	for _, c := range []struct {
		name, body string
		used, max  uint64
		ok         bool
	}{
		{"both numbers", `{"budgetUsage": 1200, "maxAmount": 5000, "other": null}`, 1200, 5000, true},
		{"zero is a number", `{"budgetUsage": 0, "maxAmount": 0}`, 0, 0, true},
		{"null usage", `{"budgetUsage": null, "maxAmount": 5000}`, 0, 0, false},
		{"null max", `{"budgetUsage": 1, "maxAmount": null}`, 0, 0, false},
		{"missing usage", `{"maxAmount": 5000}`, 0, 0, false},
		{"missing max", `{"budgetUsage": 1}`, 0, 0, false},
		{"wrong case", `{"BudgetUsage": 5, "maxAmount": 9}`, 0, 0, false},
		{"wrong case beside the key", `{"budgetUsage": null, "BUDGETUSAGE": 5, "maxAmount": 9}`, 0, 0, false},
		{"duplicate key, last wins", `{"budgetUsage": 1, "budgetUsage": 2, "maxAmount": 9}`, 2, 9, true},
		{"duplicate key, last is null", `{"budgetUsage": 1, "budgetUsage": null, "maxAmount": 9}`, 0, 0, false},
		{"renamed usage", `{"budget_usage": 1, "maxAmount": 5000}`, 0, 0, false},
		{"a string", `{"budgetUsage": "1", "maxAmount": 5000}`, 0, 0, false},
		{"negative", `{"budgetUsage": -1, "maxAmount": 5000}`, 0, 0, false},
		{"fraction", `{"budgetUsage": 1.5, "maxAmount": 5000}`, 0, 0, false},
		{"null body", `null`, 0, 0, false},
		{"empty object", `{}`, 0, 0, false},
		{"an array", `[]`, 0, 0, false},
		{"empty", ``, 0, 0, false},
	} {
		used, max, err := parseBudgetUsage([]byte(c.body))
		if (err == nil) != c.ok || used != c.used || max != c.max {
			t.Errorf("%s: %d %d %v", c.name, used, max, shownErr(err))
		}
	}
}
