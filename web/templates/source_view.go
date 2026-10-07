package templates

// SourceRow is one table row; an empty Href renders plain text.
type SourceRow struct {
	Label string
	Cells []string
	Href  string
}

// SourceTable is one chart-plus-table block (#1300); the table carries every number.
type SourceTable struct {
	Heading string
	// FirstCol heads the label column ("Type" or "Upstream").
	FirstCol string
	Blurb    string
	ChartID  string
	Cols     []string
	Rows     []SourceRow
	Chart    ChartData
}

// SourceView is the /sources/{lane} page; Upstream is set only for a multiplexing source.
type SourceView struct {
	Name     string
	Mark     string
	Blurb    string
	Types    SourceTable
	Upstream *SourceTable
	// Empty is true when the source has no completed tracks yet.
	Empty bool
}
