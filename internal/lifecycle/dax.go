package lifecycle

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
)

// Dax mirrors the public dax benchmark: a cold OpenCode clone, install, and
// typecheck inside a fresh 8 vCPU / 16 GiB sandbox, timed per phase by the
// benchmark's own script.
//
//go:embed benchmarks/dax-benchmark.sh
var daxScript []byte

const (
	// Kept across runs, so the name must stay outside the janitor's
	// api-canary-template- sweep.
	daxTemplateName  = "benchmark-dax-node22-8cpu-16gb"
	daxScriptPath    = "/tmp/dax-benchmark.sh"
	daxBuildTimeout  = 10 * time.Minute
	daxTemplateVcpu  = 8
	daxTemplateMem   = 16384
	daxTemplateDisk  = 10240
	daxTemplateImage = "node:22"
)

var daxPhases = []string{"prepare", "cache_clear", "bun_download", "bun_unpack", "clone", "install", "typecheck"}

type daxResult struct {
	Total       time.Duration
	Phases      map[string]time.Duration
	FailedPhase string
	Err         error
}

func (r Runner) runDax(ctx context.Context, runID string) (res RunResult) {
	ops := r.operations()
	telemetry := r.telemetry()
	resources := RunResources{RunID: runID, CreatedAt: r.Clock().UTC()}
	var dax daxResult
	defer func() {
		if resources.SandboxID != "" {
			res.Err = r.FinalizeSandbox(context.Background(), resources, res)
		}
		res.Report = dax.report(r.Config.Target, res)
	}()

	logStep("dax_template")
	if err := r.ensureDaxTemplate(ctx); err != nil {
		res.Err = fmt.Errorf("preparing dax template: %w", err)
		res.FailedStep = "dax_template"
		return res
	}

	createStart := r.Clock()
	logStep("create_request")
	req := r.canaryCreateSandboxRequest(resources)
	req.FromTemplate = daxTemplateName
	sb, err := ops.CreateSandbox(ctx, CreateSandboxOptions{Request: req, Telemetry: telemetry})
	ops.RecordStep(ctx, telemetry, "create_total", result(err), r.Clock().Sub(createStart))
	if err != nil {
		res.Err = err
		res.FailedStep = "create_request"
		return res
	}
	resources.SandboxID = sb.ID
	res.SandboxID = sb.ID

	// The row turns active just after create returns; file writes 409 until then.
	logStep("create_wait_active")
	if err := ops.WaitForStatusTimed(ctx, sb.ID, WaitForStatusOptions{
		Want:         "active",
		Step:         "create_wait_active",
		PollInterval: r.Config.PollInterval,
		Timeout:      r.Config.CommandTimeout,
		Telemetry:    telemetry,
	}); err != nil {
		res.Err = fmt.Errorf("waiting for sandbox to become active: %w", err)
		res.FailedStep = "create_wait_active"
		return res
	}

	logStep("upload_dax_script")
	if err := ops.WriteSandboxFileWithRetry(ctx, sb.ID, sb.AccessToken, daxScriptPath, daxScript); err != nil {
		res.Err = fmt.Errorf("uploading dax script: %w", err)
		res.FailedStep = "upload_dax_script"
		return res
	}

	logStep("dax_build")
	start := r.Clock()
	out, err := r.Client.Exec(ctx, sb.ID, sb.AccessToken, canaryapi.ExecRequest{
		Command:  fmt.Sprintf("BENCH_PROVIDER=superserve BENCH_REGION=%s bash %s", r.Config.Region, daxScriptPath),
		TimeoutS: int(daxBuildTimeout.Seconds()),
	})
	if err != nil {
		dax.Err = fmt.Errorf("running dax script after %s: %w", r.Clock().Sub(start).Round(time.Millisecond), err)
	} else {
		dax = parseDaxOutput(out)
	}
	dax.Total = r.Clock().Sub(start)
	for phase, d := range dax.Phases {
		ops.RecordStep(ctx, telemetry, "dax_"+phase, "success", d)
	}
	ops.RecordStep(ctx, telemetry, "dax_build", result(dax.Err), dax.Total)
	if dax.Err != nil {
		res.Err = dax.Err
		res.FailedStep = "dax_build"
		if dax.FailedPhase != "" {
			res.FailedStep = "dax_" + dax.FailedPhase
		}
		return res
	}
	log.Info().Dur("total", dax.Total).Interface("phases_ms", phaseMillis(dax.Phases)).Msg("dax benchmark summary")
	return res
}

// ensureDaxTemplate builds the sized template on first use and reuses it
// after; a failed build is deleted so the next run starts a fresh one.
func (r Runner) ensureDaxTemplate(ctx context.Context) error {
	for {
		tpl, found, err := r.findTemplate(ctx, daxTemplateName)
		if err != nil {
			return err
		}
		switch {
		case !found:
			if _, err := r.operations().CreateTemplate(ctx, canaryapi.CreateTemplateRequest{
				Name:      daxTemplateName,
				Vcpu:      daxTemplateVcpu,
				MemoryMib: daxTemplateMem,
				DiskMib:   daxTemplateDisk,
				BuildSpec: canaryapi.BuildSpec{From: daxTemplateImage},
			}, r.telemetry()); err != nil {
				return err
			}
		case tpl.Status == "ready":
			return nil
		case tpl.Status == "failed":
			if err := r.Client.DeleteTemplate(ctx, tpl.ID); err != nil && !errors.Is(err, canaryapi.ErrNotFound) {
				return fmt.Errorf("deleting failed template: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.Config.PollInterval):
		}
	}
}

func (r Runner) findTemplate(ctx context.Context, name string) (canaryapi.Template, bool, error) {
	templates, err := r.Client.ListTemplates(ctx, map[string]string{"owner": "team", "name_prefix": name})
	if err != nil {
		return canaryapi.Template{}, false, fmt.Errorf("listing templates: %w", err)
	}
	for _, tpl := range templates {
		if tpl.Name == name {
			return tpl, true, nil
		}
	}
	return canaryapi.Template{}, false, nil
}

// parseDaxOutput reads the script's tab-separated BENCH_* lines. A run counts
// only when it exits 0, reports no error, and prints BENCH_DONE.
func parseDaxOutput(out canaryapi.ExecResult) daxResult {
	res := daxResult{Phases: map[string]time.Duration{}}
	done := false
	var benchErr string
	for _, line := range strings.Split(out.Stdout, "\n") {
		parts := strings.Split(strings.TrimRight(line, "\r"), "\t")
		switch {
		case parts[0] == "BENCH_PHASE" && len(parts) >= 3:
			if ms, err := strconv.Atoi(parts[2]); err == nil && parts[1] != "total" {
				res.Phases[parts[1]] = time.Duration(ms) * time.Millisecond
			}
		case parts[0] == "BENCH_FAIL" && len(parts) >= 2:
			res.FailedPhase = parts[1]
		case parts[0] == "BENCH_DONE":
			done = true
		}
	}
	for _, line := range strings.Split(out.Stderr, "\n") {
		if parts := strings.Split(line, "\t"); parts[0] == "BENCH_ERROR" && len(parts) >= 2 {
			if res.FailedPhase == "" {
				res.FailedPhase = parts[1]
			}
			benchErr = strings.Join(parts[1:], ": ")
		}
	}
	// The failed phase prints its timing before BENCH_FAIL; it did not complete.
	delete(res.Phases, res.FailedPhase)
	if out.ExitCode != 0 || benchErr != "" || !done {
		res.Err = fmt.Errorf("dax script failed (exit %d, phase %q): %s", out.ExitCode, res.FailedPhase, truncate(lastLines(out.Stderr+"\n"+out.Stdout, 5), 600))
		if benchErr != "" {
			res.Err = fmt.Errorf("%w (%s)", res.Err, benchErr)
		}
	}
	return res
}

func lastLines(s string, n int) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "BENCH_") {
			keep = append(keep, line)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, " | ")
}

func phaseMillis(phases map[string]time.Duration) map[string]int64 {
	out := make(map[string]int64, len(phases))
	for k, v := range phases {
		out[k] = v.Milliseconds()
	}
	return out
}

func (d daxResult) report(target string, res RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*DAX benchmark* · %s\n", target)
	if res.Err == nil {
		fmt.Fprintf(&b, "Total *%.1fs* · passed\n", d.Total.Seconds())
	} else {
		fmt.Fprintf(&b, "*Failed* at %s\n", res.FailedStep)
	}
	var phases []string
	for _, phase := range daxPhases {
		if v, ok := d.Phases[phase]; ok {
			phases = append(phases, fmt.Sprintf("%s %.1fs", strings.ReplaceAll(phase, "_", " "), v.Seconds()))
		}
	}
	if len(phases) > 0 {
		b.WriteString(strings.Join(phases, " · "))
	}
	if res.Err != nil {
		fmt.Fprintf(&b, "\nError: %s", truncate(res.Err.Error(), 600))
	}
	return strings.TrimRight(b.String(), "\n")
}
