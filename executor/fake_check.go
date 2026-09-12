package executor

import (
	"context"

	"github.com/varvig/varvig-factory/cell"
)

// FakeChecker is a Checking for tests and the simulator: it reports a fixed fragment and
// returns scripted results without running anything. Like inference.FakeChecker it is
// in the non-test build so the full lifecycle is exercisable on a machine with
// no toolchain at all.
type FakeChecker struct {
	// Platform and Toolchains form the reported fragment.
	Platform   string
	Toolchains map[string]string
	Flags      map[string]string
	ContainerH string
	// Results maps a job name to the status it should report. A job with no
	// entry passes, so a test only has to name the checks it wants to fail.
	Results map[string]cell.Status
	// Err, if set, is returned by Run.
	Err error
	// Indescribable makes Fragment fail.
	Indescribable bool
	// Ran records the job names Run was called with, in order.
	Ran []string
}

// Name implements Checking.
func (f *FakeChecker) Name() string { return "fake" }

// Properties implements cell.Executor.
func (f *FakeChecker) Properties() cell.ExecutorProperties {
	return cell.ExecutorProperties{Deterministic: true}
}

// Fragment implements Checking.
func (f *FakeChecker) Fragment(context.Context) (cell.Fragment, error) {
	if f.Indescribable {
		return cell.Fragment{}, cell.ErrIndescribable
	}
	platform := f.Platform
	if platform == "" {
		platform = "linux/amd64"
	}
	tc := f.Toolchains
	if tc == nil {
		tc = map[string]string{"go": "1.24.7"}
	}
	return cell.Fragment{Platform: platform, Toolchains: tc, Flags: f.Flags, Container: f.ContainerH}, nil
}

// Run implements Checking.
func (f *FakeChecker) Check(_ context.Context, job Job) (Result, error) {
	f.Ran = append(f.Ran, job.Name)
	if f.Err != nil {
		return Result{}, f.Err
	}
	status, ok := f.Results[job.Name]
	if !ok {
		status = cell.StatusPass
	}
	return Result{Name: job.Name, Status: status, DurationMS: 1}, nil
}
