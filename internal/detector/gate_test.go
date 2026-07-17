package detector

import "testing"

func TestInstrumental(t *testing.T) {
	cases := []struct {
		name                         string
		music, vocalPeak, speechMean float64
		minConf, vocalMax, speechMax float64
		want                         bool
	}{
		{"all gates pass", 0.95, 0.02, 0.001, 0.90, 0.03, 0.20, true},
		{"music too low", 0.80, 0.02, 0.001, 0.90, 0.03, 0.20, false},
		{"vocal peak at threshold is not instrumental", 0.95, 0.03, 0.001, 0.90, 0.03, 0.20, false},
		{"vocal peak below threshold passes", 0.95, 0.029, 0.001, 0.90, 0.03, 0.20, true},
		{"speech too high", 0.95, 0.02, 0.25, 0.90, 0.03, 0.20, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Instrumental(c.music, c.vocalPeak, c.speechMean, c.minConf, c.vocalMax, c.speechMax); got != c.want {
				t.Fatalf("Instrumental=%v want %v", got, c.want)
			}
		})
	}
}
