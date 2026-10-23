// search_eval consumes human judgments and drives bounded, open-loop load.
// It never generates labels or marks a generation ready.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"google.golang.org/protobuf/encoding/protojson"
	"io"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	bs "live-auction-bid/backend/app/auction/service/internal/biz/buyersearch"
	"live-auction-bid/backend/app/auction/service/internal/searchindex"
	appsvc "live-auction-bid/backend/app/auction/service/internal/service"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type judgment struct {
	Query       string         `json:"query"`
	RoomID      string         `json:"roomId"`
	Grades      map[string]int `json:"grades"`
	ExpectEmpty bool           `json:"expectEmpty"`
}
type report struct {
	Mode, Generation                                              string
	Queries, Requests, Errors, Degraded                           int
	RelevantQueries, NoRelevantQueries, WrongEmptyRecommendations int
	Recall50, NDCG10, QPS, P95Ms, P99Ms                           float64
	CreatedAt                                                     time.Time
}

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "quality", "quality, ann or load")
	direct := flag.Bool("direct", false, "offline evaluation of explicit, possibly unready generation (no schema writes)")
	cases := flag.String("cases", "", "human-judged JSON array; never synthetic model labels")
	base := flag.String("url", "http://127.0.0.1:18080", "explicit search server URL")
	generation := flag.String("generation", "", "generation under test")
	qps := flag.Int("qps", 100, "open-loop target QPS (1..1000)")
	duration := flag.Duration("duration", time.Minute, "load duration <=1h")
	flag.Parse()
	if *generation == "" {
		return errors.New("generation must be recorded")
	}
	b, e := os.ReadFile(*cases)
	if e != nil {
		return e
	}
	var js []judgment
	if e = json.Unmarshal(b, &js); e != nil {
		return e
	}
	if len(js) == 0 {
		return errors.New("judgment cases empty")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	endpoint := strings.TrimRight(*base, "/") + "/api/buyer/lots/search"
	r := report{Mode: *mode, Generation: *generation, Queries: len(js), CreatedAt: time.Now()}
	var latencies []float64
	var directService *appsvc.AuctionService
	var directIndex *searchindex.HybridIndex
	var directEmbed *searchindex.EmbeddingClient
	if *direct {
		var closeDirect func()
		directService, directIndex, directEmbed, closeDirect, e = newDirectEvaluation(*generation)
		if e != nil {
			return e
		}
		defer closeDirect()
	}
	search := func(c judgment) (*v1.BuyerSearchReply, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		if directService != nil {
			out, e := directService.SearchBuyerLots(ctx, &v1.BuyerSearchRequest{Query: c.Query, RoomId: c.RoomID, PageSize: 50})
			if e != nil {
				return nil, e
			}
			if out.GetResult().GetCode() != 0 {
				return nil, errors.New("direct search failed")
			}
			return out, nil
		}
		body, e := protojson.Marshal(&v1.BuyerSearchRequest{Query: c.Query, RoomId: c.RoomID, PageSize: 50})
		if e != nil {
			return nil, e
		}
		req, e := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
		if e != nil {
			return nil, e
		}
		req.Header.Set("Content-Type", "application/json")
		response, e := client.Do(req)
		if e != nil {
			return nil, e
		}
		defer response.Body.Close()
		data, e := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
		if e != nil {
			return nil, e
		}
		out := &v1.BuyerSearchReply{}
		if e = protojson.Unmarshal(data, out); e != nil {
			return nil, e
		}
		if response.StatusCode != 200 || out.GetResult().GetCode() != 0 {
			return nil, errors.New("search request failed")
		}
		return out, nil
	}
	switch *mode {
	case "ann":
		if directIndex == nil {
			return errors.New("ANN evaluation requires -direct")
		}
		g, e := directIndex.Active(context.Background())
		if e != nil {
			return e
		}
		sum := 0.0
		for _, c := range js {
			p, e := bs.Parse(bs.Query{Text: c.Query, RoomID: c.RoomID})
			if e != nil {
				return e
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			vs, e := directEmbed.Embed(ctx, []string{p.SemanticText})
			if e != nil || len(vs) != 1 {
				cancel()
				return errors.New("ANN query embedding failed")
			}
			approx, e := directIndex.Vector(ctx, g, vs[0], p.Filters, 50)
			if e != nil {
				cancel()
				return e
			}
			exact, e := directIndex.ExactVector(ctx, g, vs[0], p.Filters, 50)
			cancel()
			if e != nil {
				return e
			}
			expected := map[string]bool{}
			for _, hit := range exact {
				expected[hit.LotID] = true
			}
			matches := 0
			for _, hit := range approx {
				if expected[hit.LotID] {
					matches++
				}
			}
			if len(exact) == 0 {
				sum++
			} else {
				sum += float64(matches) / float64(len(exact))
			}
			r.Requests++
		}
		r.Recall50 = sum / float64(len(js))
	case "quality":
		for _, c := range js {
			start := time.Now()
			out, e := search(c)
			latencies = append(latencies, float64(time.Since(start).Microseconds())/1000)
			r.Requests++
			if e != nil {
				return e
			}
			if out.Mode != "HYBRID" {
				return errors.New("quality calibration requires non-degraded hybrid mode")
			}
			ids := []string{}
			for _, h := range out.Hits {
				if !bs.PublicStatus(h.GetLot().GetStatus()) || (c.RoomID != "" && h.GetLot().GetRoomId() != c.RoomID) {
					return errors.New("visibility/scope leakage")
				}
				ids = append(ids, h.GetLot().GetId())
			}
			if c.ExpectEmpty && len(ids) != 0 {
				return errors.New("unrelated-query gate failed")
			}
			recall, ndcg, e := bs.Quality(ids, c.Grades, 50, 10)
			if e != nil {
				return e
			}
			hasRelevant := false
			for _, grade := range c.Grades {
				if grade > 0 {
					hasRelevant = true
					break
				}
			}
			if hasRelevant {
				r.RelevantQueries++
				r.Recall50 += recall
				r.NDCG10 += ndcg
			} else {
				r.NoRelevantQueries++
				if len(ids) > 0 {
					r.WrongEmptyRecommendations++
				}
			}
		}
		if r.RelevantQueries == 0 {
			return errors.New("quality dataset has no relevant-query judgments")
		}
		r.Recall50 /= float64(r.RelevantQueries)
		r.NDCG10 /= float64(r.RelevantQueries)
	case "load":
		if *qps < 1 || *qps > 1000 || *duration < time.Second || *duration > time.Hour {
			return errors.New("invalid bounded load settings")
		}
		ticker := time.NewTicker(time.Second / time.Duration(*qps))
		defer ticker.Stop()
		stop := time.NewTimer(*duration)
		defer stop.Stop()
		start := time.Now()
		var wg sync.WaitGroup
		var mu sync.Mutex
		slots := make(chan struct{}, 128)
		i := 0
	loop:
		for {
			select {
			case <-stop.C:
				break loop
			case <-ticker.C:
				i++
				select {
				case slots <- struct{}{}:
					c := js[(i-1)%len(js)]
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer func() { <-slots }()
						t := time.Now()
						out, e := search(c)
						ms := float64(time.Since(t).Microseconds()) / 1000
						mu.Lock()
						defer mu.Unlock()
						r.Requests++
						latencies = append(latencies, ms)
						if e != nil {
							r.Errors++
						} else if out.Mode != "HYBRID" {
							r.Degraded++
						}
					}()
				default:
					mu.Lock()
					r.Requests++
					r.Errors++
					mu.Unlock()
				}
			}
		}
		wg.Wait()
		r.QPS = float64(r.Requests) / time.Since(start).Seconds()
	default:
		return errors.New("unknown evaluation mode")
	}
	sort.Float64s(latencies)
	if len(latencies) > 0 {
		r.P95Ms = latencies[(len(latencies)-1)*95/100]
		r.P99Ms = latencies[(len(latencies)-1)*99/100]
	}
	return json.NewEncoder(os.Stdout).Encode(r)
}
