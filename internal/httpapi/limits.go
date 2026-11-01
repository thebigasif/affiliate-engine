package httpapi

// Input bounds shared by the admin handlers. Each is far above any real
// value; they only stop a typo or a hostile client from storing a value
// that breaks later arithmetic or a database column.
const (
	// maxFixedRateMinor caps a fixed commission (minor units: 1e11 cents is
	// one billion dollars). Sums of fixed commissions stay far from the
	// int64 limit.
	maxFixedRateMinor = 100_000_000_000

	// maxRulePriority bounds a rule's priority: the Postgres column is a
	// 32-bit INTEGER, so a larger value failed with a 500.
	maxRulePriority = 1_000_000

	// maxCampaignNameRunes caps a program name.
	maxCampaignNameRunes = 120

	// maxNoteRunes caps a fraud-review note, like a rejection reason.
	maxNoteRunes = 1000
)
