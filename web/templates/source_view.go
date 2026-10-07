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
	Trend TrendView
}

// TrendSeries is one time-chart series; a nil point is a gap (JSON null).
type TrendSeries struct {
	Label string     `json:"label"`
	Data  []*float64 `json:"data"`
}

// SeriesData is a multi-series time chart: Labels are the UTC day strings.
type SeriesData struct {
	Labels []string
	Series []TrendSeries
}

// SeriesJSON serializes the series for the data-chart-series attribute.
func (s SeriesData) SeriesJSON() string { return marshalJSON(s.Series) }

// HasPoint reports whether any series has a non-nil point. A real zero is data
// here (a hit rate of 0% on a day of all misses), unlike HasData.
func (s SeriesData) HasPoint() bool {
	for _, se := range s.Series {
		for _, p := range se.Data {
			if p != nil {
				return true
			}
		}
	}
	return false
}

// HasData reports whether any series has a non-nil, non-zero point: for the
// delivered-types chart, where all zeros means nothing landed.
func (s SeriesData) HasData() bool {
	for _, se := range s.Series {
		for _, p := range se.Data {
			if p != nil && *p != 0 {
				return true
			}
		}
	}
	return false
}

// TrendRange is one range link; Current marks the active one.
type TrendRange struct {
	Label   string
	Href    string
	Current bool
}

// TrendView is the over-time section of a source page (#1302). Note, when set,
// replaces the charts (no history, or a group that has no events).
type TrendView struct {
	Note      string
	Ranges    []TrendRange
	Days      int
	Hit       SeriesData
	Types     SeriesData
	TableRows []SourceRow
}
