// leakdrv runs terminal execs whose client never closes stdin and reports
// the shim's open file descriptors and goroutines after each batch.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "leakdrv:", err)
		os.Exit(1)
	}
}

func main() {
	addr := flag.String("address", "", "containerd socket")
	image := flag.String("image", "docker.io/library/busybox:latest", "image")
	n := flag.Int("n", 200, "terminal execs to run")
	every := flag.Int("every", 25, "measure every N execs")
	mode := flag.String("mode", "noclose", "noclose: client closes stdin after delete; hold: client never closes stdin; closeio: client calls CloseIO; nostdin: no stdin stream")
	logPath := flag.String("log", "", "containerd log (for SIGUSR1 goroutine dumps)")
	flag.Parse()

	ctx := namespaces.WithNamespace(context.Background(), "leaktest")
	c, err := client.New(*addr)
	must(err)
	defer c.Close()
	img, err := c.GetImage(ctx, *image)
	if err != nil {
		img, err = c.Pull(ctx, *image, client.WithPullUnpack)
		must(err)
	}
	id := fmt.Sprintf("leak-%d", time.Now().UnixNano())
	ctr, err := c.NewContainer(ctx, id, client.WithNewSnapshot(id+"-snap", img),
		client.WithNewSpec(oci.WithImageConfig(img), oci.WithProcessArgs("sleep", "3600")))
	must(err)
	defer ctr.Delete(ctx, client.WithSnapshotCleanup)
	task, err := ctr.NewTask(ctx, cio.NullIO)
	must(err)
	defer func() {
		_ = task.Kill(ctx, syscall.SIGKILL)
		_, _ = task.Delete(ctx, client.WithProcessKill)
	}()
	must(task.Start(ctx))
	shim := findShim(id)
	spec, err := ctr.Spec(ctx)
	must(err)
	pspec := spec.Process
	// Live long enough for the console wiring to finish and for output to
	// cross the pty, like an interactive session that then exits.
	pspec.Args = []string{"sh", "-c", "echo ready; sleep 0.4; echo bye"}
	pspec.Terminal = true

	var held []io.Closer
	report := func(label string) {
		fmt.Printf("%-16s shim=%d fds=%d goroutines=%d  %s\n", label, shim, fds(shim), goroutines(shim, *logPath), fdKinds(shim))
	}
	report("baseline")
	for i := 1; i <= *n; i++ {
		pr, pw := io.Pipe() // a client stdin that never reaches EOF while the exec runs
		var stdin io.Reader = pr
		if *mode == "nostdin" {
			stdin = nil
		}
		execID := "e" + strconv.Itoa(i)
		p, err := task.Exec(ctx, execID, pspec, cio.NewCreator(cio.WithStreams(stdin, io.Discard, nil), cio.WithTerminal))
		must(err)
		exited, err := p.Wait(ctx)
		must(err)
		must(p.Start(ctx))
		<-exited
		if *mode == "closeio" {
			must(p.CloseIO(ctx, client.WithStdinCloser))
		}
		_, err = p.Delete(ctx)
		must(err)
		switch *mode {
		case "hold":
			held = append(held, pw) // a client that keeps its stdin open for good
		default:
			pw.Close() // the client lets go of its writer only after delete
		}
		if i%*every == 0 {
			report(fmt.Sprintf("after %d", i))
		}
	}
	time.Sleep(time.Second)
	report("settled")
	leftovers(shim)
	for _, h := range held {
		h.Close()
	}
	if len(held) > 0 {
		time.Sleep(time.Second)
		report("client gone")
	}
}

// fdKinds summarizes what the shim's descriptors point at.
func fdKinds(pid int) string {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "?"
	}
	kinds := map[string]int{}
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		target = strings.TrimSuffix(target, " (deleted)")
		switch {
		case strings.Contains(target, "ptmx"):
			kinds["ptmx"]++
		case strings.HasSuffix(target, "stdin"):
			kinds["stdin-fifo"]++
		case strings.HasSuffix(target, "stdout"), strings.HasSuffix(target, "stderr"):
			kinds["out-fifo"]++
		case strings.HasPrefix(target, "pipe:"):
			kinds["pipe"]++
		case strings.HasPrefix(target, "socket:"), strings.HasPrefix(target, "anon_inode:"):
			kinds["sock/anon"]++
		default:
			kinds["other"]++
		}
	}
	parts := []string{}
	for _, k := range []string{"ptmx", "stdin-fifo", "out-fifo", "pipe", "sock/anon", "other"} {
		if kinds[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", k, kinds[k]))
		}
	}
	return strings.Join(parts, " ")
}

// findShim returns the pid of the containerd-shim-runc-v2 serving container id.
func findShim(id string) int {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmd := strings.ReplaceAll(string(raw), "\x00", " ")
		if strings.Contains(cmd, "containerd-shim-runc-v2") && strings.Contains(cmd, "-id "+id) {
			return pid
		}
	}
	must(fmt.Errorf("no shim found for %s", id))
	return 0
}

func fds(pid int) int {
	entries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		return -1
	}
	return len(entries)
}

// goroutines asks the shim for its stack dump (SIGUSR1) and counts the
// goroutines in what containerd relays to its log.
func goroutines(pid int, logPath string) int {
	if logPath == "" {
		return -1
	}
	before, _ := os.Stat(logPath)
	_ = syscall.Kill(pid, syscall.SIGUSR1)
	time.Sleep(400 * time.Millisecond)
	f, err := os.Open(logPath)
	if err != nil {
		return -1
	}
	defer f.Close()
	if before != nil {
		f.Seek(before.Size(), io.SeekStart)
	}
	data, _ := io.ReadAll(f)
	return len(goroutineRe.FindAll(data, -1))
}

var goroutineRe = regexp.MustCompile(`goroutine \d+ \[`)

// leftovers prints every descriptor target of the shim that is not part of
// its baseline set, so a leak names the kind of handle it kept.
func leftovers(pid int) {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(target, "ptmx") || strings.Contains(target, "stdin") || strings.Contains(target, "stdout") || strings.Contains(target, "stderr") {
			fmt.Printf("    fd %s -> %s\n", e.Name(), target)
		}
	}
}
