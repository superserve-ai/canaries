package lifecycle

import (
	"errors"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
)

func TestSummarizeBurstScoresLikeThePublicBenchmark(t *testing.T) {
	// 20 samples trim one from each end; the 9000ms outlier must not count.
	var samples []burstSample
	for i := 1; i <= 19; i++ {
		samples = append(samples, burstSample{TTI: time.Duration(i*100) * time.Millisecond, ReadyAt: time.Duration(i*100) * time.Millisecond})
	}
	samples = append(samples, burstSample{TTI: 9000 * time.Millisecond, ReadyAt: 9000 * time.Millisecond})
	samples = append(samples, burstSample{Err: errors.New("create failed")})

	s := summarizeBurst(samples)
	if s.Median != 1050*time.Millisecond || s.P95 != 1900*time.Millisecond || s.P99 != 1900*time.Millisecond {
		t.Fatalf("stats = %v/%v/%v, want 1.05s/1.9s/1.9s", s.Median, s.P95, s.P99)
	}
	// timing = .60*89.5 + .25*81 + .15*81 = 86.1, times 20/21 succeeded.
	if s.Score != 82 {
		t.Fatalf("score = %v, want 82", s.Score)
	}
	if s.Succeeded != 20 || s.WallClock != 9*time.Second || s.FirstReady != 100*time.Millisecond {
		t.Fatalf("summary = %+v", s)
	}
}

func TestParseDaxOutput(t *testing.T) {
	ok := parseDaxOutput(canaryapi.ExecResult{Stdout: "BENCH_PHASE\tprepare\t4900\nBENCH_PHASE\tclone\t6600\nBENCH_DONE\tabc\nBENCH_PHASE\ttotal\t11500\n"})
	if ok.Err != nil || ok.Phases["prepare"] != 4900*time.Millisecond || ok.Phases["clone"] != 6600*time.Millisecond || len(ok.Phases) != 2 {
		t.Fatalf("success parse = %+v", ok)
	}

	// The failed phase prints its timing before BENCH_FAIL, so it must be dropped.
	failed := parseDaxOutput(canaryapi.ExecResult{
		ExitCode: 1,
		Stdout:   "BENCH_PHASE\tprepare\t4900\nBENCH_PHASE\tinstall\t300\nBENCH_FAIL\tinstall\n",
		Stderr:   "error: lockfile changed\n",
	})
	if failed.Err == nil || failed.FailedPhase != "install" || len(failed.Phases) != 1 {
		t.Fatalf("failure parse = %+v", failed)
	}

	if noDone := parseDaxOutput(canaryapi.ExecResult{Stdout: "BENCH_PHASE\tprepare\t4900\n"}); noDone.Err == nil {
		t.Fatal("output without BENCH_DONE must fail")
	}
}
