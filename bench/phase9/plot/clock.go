package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// The clock guard. Every number the experiments report is a duration or a
// rate measured by the kernel's clock, and the demos' timelines join
// wall-clock times from k6, the proxy's log and the scripts. On the WSL2
// machine these results were first taken on, two time daemons fought over
// the shared kernel clock: the kernel tick was left at 10833 µs instead of
// 10000, so every clock ran 8.3% fast, and the wall clock was stepped back
// about 2.5 s every 30 s (docs/DESIGN.md, Phase 9). Neither is visible in
// the results themselves, so the scripts check before and after each run.

// maxTickError is how far the kernel tick may be from nominal, in µs per
// 10,000: 10 is 1,000 ppm. NTP daemons slew within ±500 ppm through the
// frequency and only use the tick past that.
const maxTickError = 10

// maxStep is how far the wall clock may move against the monotonic clock
// during a run.
const maxStep = 100 * time.Millisecond

// tick returns the kernel's tick (µs per clock tick at USER_HZ 100) and
// frequency offset in ppm.
func tick() (int64, float64, error) {
	var t syscall.Timex
	if _, err := syscall.Adjtimex(&t); err != nil {
		return 0, 0, fmt.Errorf("adjtimex: %w", err)
	}
	return int64(t.Tick), float64(t.Freq) / 65536, nil
}

// CheckTick fails when the tick is far from 10000.
func CheckTick(tk int64, freq float64) error {
	if d := tk - 10000; d > maxTickError || d < -maxTickError {
		return fmt.Errorf("kernel tick is %d, not 10000: every clock runs %+.1f%% off (frequency %+.1f ppm); "+
			"another time daemon is adjusting the clock (see docs/DESIGN.md, Phase 9)", tk, float64(d)/100, freq)
	}
	return nil
}

// offset is the wall clock minus CLOCK_BOOTTIME, in nanoseconds: the wall
// time of boot. It is the same in every process and container on the
// kernel, and moves only when the wall clock is stepped (slewing moves it
// by microseconds), so two readings taken by different processes show a
// step between them.
func offset() (int64, error) {
	var ts syscall.Timespec
	// CLOCK_BOOTTIME = 7; no stdlib wrapper, so the raw call.
	if _, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 7, uintptr(unsafe.Pointer(&ts)), 0); e != 0 {
		return 0, fmt.Errorf("clock_gettime: %w", e)
	}
	wall := time.Now().UnixNano()
	return wall - ts.Nano(), nil
}

// CheckStep fails when two offsets differ by more than maxStep.
func CheckStep(before, after int64) error {
	d := time.Duration(after - before)
	if math.Abs(float64(d)) > float64(maxStep) {
		return fmt.Errorf("the wall clock was stepped by %v during the run, so its timeline is not trustworthy", d.Round(time.Millisecond))
	}
	return nil
}

// cmdClock: plot clock -save FILE checks the tick and saves the wall-clock
// offset; plot clock -check FILE checks the tick again and that the wall
// clock was not stepped since -save.
func cmdClock(args []string) error {
	fs := flag.NewFlagSet("clock", flag.ExitOnError)
	save := fs.String("save", "", "check the tick and save the wall-clock offset here")
	check := fs.String("check", "", "check the tick and the offset against the one saved here")
	fs.Parse(args)
	if (*save == "") == (*check == "") {
		return errors.New("clock: want exactly one of -save and -check")
	}
	tk, freq, err := tick()
	if err != nil {
		return err
	}
	if err := CheckTick(tk, freq); err != nil {
		return err
	}
	off, err := offset()
	if err != nil {
		return err
	}
	if *save != "" {
		return os.WriteFile(*save, []byte(strconv.FormatInt(off, 10)+"\n"), 0o644)
	}
	b, err := os.ReadFile(*check)
	if err != nil {
		return err
	}
	before, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return fmt.Errorf("%s: %w", *check, err)
	}
	return CheckStep(before, off)
}
