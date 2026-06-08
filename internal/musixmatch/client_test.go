package musixmatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestClientName(t *testing.T) {
	if got := NewClient("token").Name(); got != "musixmatch" {
		t.Fatalf("Name() = %q; want musixmatch", got)
	}
}

func TestFindLyricsBuildsRequestAndParsesSyncedLyrics(t *testing.T) {
	client := NewClient("test-token")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatalf("method = %q; want GET", req.Method)
		}
		if got := req.URL.Query().Get("usertoken"); got != "test-token" {
			t.Fatalf("usertoken = %q; want test-token", got)
		}
		if got := req.URL.Query().Get("q_artist"); got != "artist" {
			t.Fatalf("q_artist = %q; want artist", got)
		}
		if got := req.URL.Query().Get("q_track"); got != "title" {
			t.Fatalf("q_track = %q; want title", got)
		}
		return jsonResponse(http.StatusOK, `{
			"message": {
				"header": {"status_code": 200},
				"body": {
					"macro_calls": {
						"matcher.track.get": {
							"message": {
								"header": {"status_code": 200},
								"body": {
									"track": {
										"track_name": "title",
										"artist_name": "artist",
										"album_name": "album",
										"has_subtitles": 1,
										"has_lyrics": 1
									}
								}
							}
						},
						"track.lyrics.get": {"message": {"body": {}}},
						"track.subtitles.get": {
							"message": {
								"body": {
									"subtitle_list": [
										{
											"subtitle": {
												"subtitle_body": "[{\"text\":\"line one\",\"time\":{\"total\":1.23,\"minutes\":0,\"seconds\":1,\"hundredths\":23}}]"
											}
										}
									]
								}
							}
						}
					}
				}
			}
		}`), nil
	})}

	song, err := client.FindLyrics(context.Background(), models.Track{
		TrackName:  "title",
		ArtistName: "artist",
		AlbumName:  "album",
	})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.Track.TrackName != "title" {
		t.Fatalf("track name = %q; want title", song.Track.TrackName)
	}
	if len(song.Subtitles.Lines) != 1 {
		t.Fatalf("subtitle lines = %d; want 1", len(song.Subtitles.Lines))
	}
	if got := song.Subtitles.Lines[0].Text; got != "line one" {
		t.Fatalf("subtitle text = %q; want line one", got)
	}
}

func TestFindLyricsTruncatedSubtitleBodyReturnsErrTruncatedResponse(t *testing.T) {
	emptyBody := `{
		"message": {
			"header": {"status_code": 200},
			"body": {
				"macro_calls": {
					"matcher.track.get": {
						"message": {
							"header": {"status_code": 200},
							"body": {
								"track": {
									"track_name": "title",
									"artist_name": "artist",
									"has_subtitles": 1
								}
							}
						}
					},
					"track.lyrics.get": {"message": {"body": {}}},
					"track.subtitles.get": {
						"message": {
							"body": {
								"subtitle_list": [
									{"subtitle": {"subtitle_body": ""}}
								]
							}
						}
					}
				}
			}
		}
	}`
	absentBody := `{
		"message": {
			"header": {"status_code": 200},
			"body": {
				"macro_calls": {
					"matcher.track.get": {
						"message": {
							"header": {"status_code": 200},
							"body": {
								"track": {
									"track_name": "title",
									"artist_name": "artist",
									"has_subtitles": 1
								}
							}
						}
					},
					"track.lyrics.get": {"message": {"body": {}}},
					"track.subtitles.get": {
						"message": {
							"body": {
								"subtitle_list": [
									{"subtitle": {}}
								]
							}
						}
					}
				}
			}
		}
	}`
	cases := map[string]string{
		"empty subtitle_body":  emptyBody,
		"absent subtitle_body": absentBody,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(http.StatusOK, body)
			_, err := client.FindLyrics(context.Background(), models.Track{TrackName: "title", ArtistName: "artist"})
			if !errors.Is(err, ErrTruncatedResponse) {
				t.Fatalf("FindLyrics error = %v; want errors.Is(_, ErrTruncatedResponse)", err)
			}
		})
	}
}

func TestFindLyricsParsesUnsyncedLyrics(t *testing.T) {
	client := newTestClient(http.StatusOK, `{
		"message": {
			"header": {"status_code": 200},
			"body": {
				"macro_calls": {
					"matcher.track.get": {
						"message": {
							"header": {"status_code": 200},
							"body": {
								"track": {
									"track_name": "title",
									"artist_name": "artist",
									"has_subtitles": 0,
									"has_lyrics": 1
								}
							}
						}
					},
					"track.lyrics.get": {
						"message": {
							"body": {
								"lyrics": {
									"lyrics_body": "plain lyrics",
									"restricted": 0
								}
							}
						}
					},
					"track.subtitles.get": {"message": {"body": {}}}
				}
			}
		}
	}`)

	song, err := client.FindLyrics(context.Background(), models.Track{TrackName: "title", ArtistName: "artist"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if got := song.Lyrics.LyricsBody; got != "plain lyrics" {
		t.Fatalf("lyrics body = %q; want plain lyrics", got)
	}
}

func TestFindLyricsAcceptsInstrumentalTrack(t *testing.T) {
	client := newTestClient(http.StatusOK, `{
		"message": {
			"header": {"status_code": 200},
			"body": {
				"macro_calls": {
					"matcher.track.get": {
						"message": {
							"header": {"status_code": 200},
							"body": {
								"track": {
									"track_name": "instrumental",
									"artist_name": "artist",
									"has_subtitles": 0,
									"has_lyrics": 0,
									"instrumental": 1
								}
							}
						}
					},
					"track.lyrics.get": {"message": {"body": {}}},
					"track.subtitles.get": {"message": {"body": {}}}
				}
			}
		}
	}`)

	song, err := client.FindLyrics(context.Background(), models.Track{TrackName: "instrumental", ArtistName: "artist"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.Track.Instrumental != 1 {
		t.Fatalf("instrumental = %d; want 1", song.Track.Instrumental)
	}
}

func TestFindLyricsErrors(t *testing.T) {
	tests := map[string]struct {
		client  *Client
		wantErr string
	}{
		"http unauthorized": {
			client:  newTestClient(http.StatusUnauthorized, ""),
			wantErr: "unauthorized",
		},
		"http too many requests": {
			client:  newTestClient(http.StatusTooManyRequests, ""),
			wantErr: "rate limited",
		},
		"http not found": {
			client:  newTestClient(http.StatusNotFound, ""),
			wantErr: "no results found",
		},
		"invalid token body": {
			client: newTestClient(http.StatusOK, `{
				"message": {
					"header": {
						"status_code": 401,
						"hint": "renew"
					}
				}
			}`),
			wantErr: "token renewal required",
		},
		"restricted lyrics": {
			client: newTestClient(http.StatusOK, `{
				"message": {
					"header": {"status_code": 200},
					"body": {
						"macro_calls": {
							"matcher.track.get": {
								"message": {
									"header": {"status_code": 200},
									"body": {
										"track": {
											"track_name": "title",
											"artist_name": "artist",
											"has_subtitles": 0,
											"has_lyrics": 1
										}
									}
								}
							},
							"track.lyrics.get": {
								"message": {
									"body": {
										"lyrics": {"restricted": 1}
									}
								}
							},
							"track.subtitles.get": {"message": {"body": {}}}
						}
					}
				}
			}`),
			wantErr: "restricted",
		},
		"missing track": {
			client: newTestClient(http.StatusOK, `{
				"message": {
					"header": {"status_code": 200},
					"body": {
						"macro_calls": {
							"matcher.track.get": {
								"message": {
									"header": {"status_code": 200},
									"body": {}
								}
							},
							"track.lyrics.get": {"message": {"body": {}}},
							"track.subtitles.get": {"message": {"body": {}}}
						}
					}
				}
			}`),
			wantErr: "missing track data",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := tt.client.FindLyrics(context.Background(), models.Track{TrackName: "title", ArtistName: "artist"})
			if err == nil {
				t.Fatal("FindLyrics returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q; want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestFindLyricsReturnsSentinelErrors(t *testing.T) {
	tests := map[string]struct {
		status   int
		sentinel error
	}{
		"401 unauthorized":      {http.StatusUnauthorized, ErrUnauthorized},
		"429 too many requests": {http.StatusTooManyRequests, ErrRateLimited},
		"404 not found":         {http.StatusNotFound, ErrNotFound},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(tt.status, "")
			_, err := client.FindLyrics(context.Background(), models.Track{TrackName: "title", ArtistName: "artist"})
			if err == nil {
				t.Fatal("FindLyrics returned nil error")
			}
			if !errors.Is(err, tt.sentinel) {
				t.Fatalf("error = %v; want errors.Is(_, %v)", err, tt.sentinel)
			}
		})
	}
}

func TestFindLyricsBenignMissesAreClassifiable(t *testing.T) {
	restricted := `{
		"message": {"header": {"status_code": 200}, "body": {"macro_calls": {
			"matcher.track.get": {"message": {"header": {"status_code": 200}, "body": {"track": {"track_name": "title", "artist_name": "artist", "has_subtitles": 0, "has_lyrics": 1}}}},
			"track.lyrics.get": {"message": {"body": {"lyrics": {"restricted": 1}}}},
			"track.subtitles.get": {"message": {"body": {}}}
		}}}
	}`
	missingLyrics := `{
		"message": {"header": {"status_code": 200}, "body": {"macro_calls": {
			"matcher.track.get": {"message": {"header": {"status_code": 200}, "body": {"track": {"track_name": "title", "artist_name": "artist", "has_subtitles": 0, "has_lyrics": 1}}}},
			"track.lyrics.get": {"message": {"body": {}}},
			"track.subtitles.get": {"message": {"body": {}}}
		}}}
	}`
	noLyrics := `{
		"message": {"header": {"status_code": 200}, "body": {"macro_calls": {
			"matcher.track.get": {"message": {"header": {"status_code": 200}, "body": {"track": {"track_name": "title", "artist_name": "artist", "has_subtitles": 0, "has_lyrics": 0}}}},
			"track.lyrics.get": {"message": {"body": {}}},
			"track.subtitles.get": {"message": {"body": {}}}
		}}}
	}`

	tests := map[string]struct {
		client   *Client
		sentinel error
	}{
		"http 404":            {newTestClient(http.StatusNotFound, ""), ErrNotFound},
		"restricted lyrics":   {newTestClient(http.StatusOK, restricted), ErrNoLyrics},
		"missing lyrics data": {newTestClient(http.StatusOK, missingLyrics), ErrNoLyrics},
		"no lyrics found":     {newTestClient(http.StatusOK, noLyrics), ErrNoLyrics},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := tt.client.FindLyrics(context.Background(), models.Track{TrackName: "title", ArtistName: "artist"})
			if err == nil {
				t.Fatal("FindLyrics returned nil error")
			}
			if !errors.Is(err, tt.sentinel) {
				t.Fatalf("error = %v; want errors.Is(_, %v)", err, tt.sentinel)
			}
			if !IsBenignMiss(err) {
				t.Fatalf("IsBenignMiss(%v) = false; want true (benign terminal miss)", err)
			}
		})
	}
}

func TestFindLyricsInnerMatcher401ReturnsErrUnauthorized(t *testing.T) {
	// HTTP 200 with an inner matcher.track.get status_code of 401 -- the bare-401
	// throttle path inside the macro response (distinct from the header-level
	// hint=renew path, which returns ErrTokenRenewalRequired).
	body := `{"message": {"header": {"status_code": 200}, "body": {"macro_calls": {
		"matcher.track.get": {"message": {"header": {"status_code": 401}}},
		"track.lyrics.get": {"message": {"body": {}}},
		"track.subtitles.get": {"message": {"body": {}}}
	}}}}`
	client := newTestClient(http.StatusOK, body)
	_, err := client.FindLyrics(context.Background(), models.Track{TrackName: "title", ArtistName: "artist"})
	if err == nil {
		t.Fatal("FindLyrics returned nil error for inner matcher 401")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v; want errors.Is(_, ErrUnauthorized)", err)
	}
	if errors.Is(err, ErrTokenRenewalRequired) {
		t.Fatalf("error = %v; a bare inner 401 must NOT be classified as a renewal", err)
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error = %q; want non-asserting 'HTTP 401' wording", err.Error())
	}
}

func TestTokenRenewalErrorIsBothSentinels(t *testing.T) {
	err := fmt.Errorf("wrap: %w", ErrTokenRenewalRequired)
	if !errors.Is(err, ErrTokenRenewalRequired) {
		t.Fatal("want errors.Is(_, ErrTokenRenewalRequired) so the worker can distinguish a genuine renewal")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatal("want errors.Is(_, ErrUnauthorized) so the circuit breaker still trips on renewal")
	}
	if IsBenignMiss(err) {
		t.Fatal("IsBenignMiss(renewal) = true; want false (renewal is not a benign miss)")
	}
}

func TestIsBenignMissRejectsGenuineErrors(t *testing.T) {
	for name, err := range map[string]error{
		"unauthorized": ErrUnauthorized,
		"rate limited": ErrRateLimited,
		"transport":    errors.New("network down"),
		"nil":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			if IsBenignMiss(err) {
				t.Fatalf("IsBenignMiss(%v) = true; want false", err)
			}
		})
	}
}

func TestFindLyricsInBodyInvalidTokenReturnsErrUnauthorized(t *testing.T) {
	body := `{"message": {"header": {"status_code": 401, "hint": "renew"}}}`
	client := newTestClient(http.StatusOK, body)
	_, err := client.FindLyrics(context.Background(), models.Track{TrackName: "title", ArtistName: "artist"})
	if err == nil {
		t.Fatal("FindLyrics returned nil error for in-body 401/renew")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v; want errors.Is(_, ErrUnauthorized) so circuit breaker keys off it", err)
	}
	if !errors.Is(err, ErrTokenRenewalRequired) {
		t.Fatalf("error = %v; want errors.Is(_, ErrTokenRenewalRequired) so the worker can treat hint=renew as a genuine auth failure", err)
	}
}

func TestFindLyricsReturnsTransportError(t *testing.T) {
	wantErr := errors.New("network down")
	client := NewClient("token")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, wantErr
	})}

	_, err := client.FindLyrics(context.Background(), models.Track{TrackName: "title", ArtistName: "artist"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("FindLyrics error = %v; want %v", err, wantErr)
	}
}

func newTestClient(status int, body string) *Client {
	client := NewClient("token")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(status, body), nil
	})}
	return client
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
