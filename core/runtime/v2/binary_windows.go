/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package v2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/containerd/log"
)

// readShimBootstrap starts cmd and collects the shim "start" helper's
// bootstrap response.
//
// On Windows we support self-daemonizing shims (Detachable): the helper
// writes its result, closes stdout to signal readiness, and stays alive as
// the long-lived TTRPC server. CombinedOutput would block indefinitely in
// that case. Instead we race two goroutines — one draining stdout, one
// waiting on the process — and proceed as soon as either delivers enough
// information to make a decision.
//
// The helper's exit code is not the source of truth for shim health; the
// TTRPC connection attempted by the caller is.
//
// We use os.Pipe rather than cmd.StdoutPipe so that cmd.Wait does not close
// the read end while ReadAll is still in flight (per Go docs, calling Wait
// before StdoutPipe reads complete is incorrect).
func readShimBootstrap(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("shim start pipe: %w", err)
	}
	cmd.Stdout = w
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, fmt.Errorf("start shim: %w", err)
	}
	// Close the parent's write end; the child has its own dup. This ensures
	// ReadAll sees EOF when the child closes its copy (or exits).
	w.Close()

	type readResult struct {
		data []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	go func() {
		defer r.Close()
		data, err := io.ReadAll(r)
		readCh <- readResult{data, err}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case rr := <-readCh:
		response := bytes.TrimSpace(rr.data)
		if len(response) == 0 || rr.err != nil {
			// Empty or broken stdout: pull the exit code to build a useful
			// error. Bound the wait in case the helper closed stdout but is
			// still alive (broken self-daemonizing shim).
			select {
			case waitErr := <-waitCh:
				return nil, fmt.Errorf("shim start: %w (read: %v)\nstderr: %s",
					waitErr, rr.err, stderrBuf.String())
			case <-time.After(5 * time.Second):
				cmd.Process.Kill() //nolint:errcheck
				<-waitCh
				return nil, fmt.Errorf("shim start: empty bootstrap and helper did not exit within 5s"+
					"\nread err: %v\nstderr: %s", rr.err, stderrBuf.String())
			}
		}
		// Good response — reap the helper in the background.
		go func() {
			if waitErr := <-waitCh; waitErr != nil {
				log.G(ctx).WithError(waitErr).Debug(
					"shim start helper exited with error after writing bootstrap")
			}
		}()
		return response, nil

	case waitErr := <-waitCh:
		// Helper exited before stdout finished draining. Collect whatever it
		// managed to write.
		rr := <-readCh
		response := bytes.TrimSpace(rr.data)
		if len(response) == 0 {
			return nil, fmt.Errorf("shim start: %w\nstderr: %s",
				waitErr, stderrBuf.String())
		}
		if waitErr != nil {
			return nil, fmt.Errorf("shim start: %w\nstdout: %s\nstderr: %s",
				waitErr, response, stderrBuf.String())
		}
		// Clean exit with output: classic shim happy path.
		return response, nil

	}
}
