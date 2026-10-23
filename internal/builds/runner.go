package builds

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// SubprocessRunner runs the docs engine's v1 build inside a checked-out
// engine directory: sync the tenant's content tree, then the two astro builds
// — the public docs site and the private notes site. The three commands are
// the engine's own interface (anywaye/docs `package.json`: `sync`,
// `build:public`, `build:notes`), read here rather than restated, so an
// engine rename breaks loudly at the failing step instead of building half a
// site quietly.
//
// The step that fails names itself in the error, and the steps after it do
// not run: a build that synced but never compiled is a failed build with the
// compiler's reason, not a succeeded build with a stale dist.
type SubprocessRunner struct {
	// run is the exec seam, so tests assert the exact argv without a node
	// toolchain. Production is exec.CommandContext in engineDir.
	run func(ctx context.Context, dir, name string, args ...string) error
}

// NewSubprocessRunner builds the production runner.
func NewSubprocessRunner() *SubprocessRunner {
	return &SubprocessRunner{run: runCommand}
}

func runCommand(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The tail, not the whole log: the build log lives in the process
		// output, and the recorded reason is what a tenant reads. A failure
		// with no output at all still names the command.
		const tail = 4 << 10
		if len(out) > tail {
			out = out[len(out)-tail:]
		}
		return fmt.Errorf("%s: %w: %s", shellLine(name, args), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shellLine(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

// Build installs the engine's dependencies, then syncs, then runs the public
// and notes builds. `npm ci` is not incidental: a fresh engine checkout has
// no node_modules and the astro binary is not on PATH until it does — the
// first tenant build failed on exactly that, in a sandbox where every earlier
// build had primed its tree.
func (r *SubprocessRunner) Build(ctx context.Context, engineDir, contentDir string) error {
	steps := [][]string{
		{"npm", "ci"},
		{"node", "bin/sync-content", "--source", contentDir},
		{"npm", "run", "build:public"},
		{"npm", "run", "build:notes"},
	}
	for _, step := range steps {
		if err := r.run(ctx, engineDir, step[0], step[1:]...); err != nil {
			return fmt.Errorf("builds: %s: %w", shellLine(step[0], step[1:]), err)
		}
	}
	return nil
}
