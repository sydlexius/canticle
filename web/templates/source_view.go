package templates

// SourceRow is one table row.
type SourceRow struct {
	Label string
	Cells []string
}

// SourceTable is one tiles-plus-doughnut block (#1300); Rows, when set, add a
// per-type table (the By upstream block) that the tiles cannot carry.
type SourceTable struct {
	Heading string
	Blurb   string
	ChartID string
	Tiles   []StatTile
	Chart   ChartData
	// FirstCol, Cols and Rows describe the optional table.
	FirstCol string
	Cols     []string
	Rows     []SourceRow
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
	// Details is an optional per-day tooltip line, parallel to Labels.
	Details []string
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
	Hit       SeriesData
	Types     SeriesData
	TableRows []SourceRow
}
