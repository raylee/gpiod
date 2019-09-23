// SPDX-License-Identifier: MIT
//
// Copyright © 2019 Kent Gibson <warthog618@gmail.com>.

// +build linux

// Package gpiod provides a library for the Linux GPIO descriptor UAPI.
//
// This is a Go equivalent of libgpiod.
package gpiod

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/warthog618/gpiod/uapi"
	"golang.org/x/sys/unix"
)

// Chip represents a single GPIO chip that controls a set of lines.
type Chip struct {
	f *os.File
	// The system name for this chip.
	Name string

	// A more individual label for the chip.
	Label string

	// The number of GPIO lines on this chip.
	lines int

	// default consumer label for reserved lines
	consumer string

	// mutex covers the attributes below it.
	mu sync.Mutex

	// indicates the chip has been closed.
	closed bool
}

// LineInfo contains a summary of publically available information about the
// line.
type LineInfo struct {
	// The line offset within the chip.
	Offset int

	// The system name for the line.
	Name string

	// A string identifying the requester of the line, if requested.
	Consumer string

	// True if the line is requested.
	Requested bool

	// True if the line was requested as an output.
	IsOut bool

	// True if the line was requested as active low.
	ActiveLow bool

	// True if the line was requested as open drain.
	//
	// Only valid for outputs.
	OpenDrain bool

	// True if the line was requested as open source.
	//
	// Only valid for outputs.
	OpenSource bool
}

// Chips returns the names of the available GPIO devices.
func Chips() []string {
	cc := []string(nil)
	for _, name := range chipNames() {
		if IsChip(name) == nil {
			cc = append(cc, name)
		}
	}
	return cc
}

// FindLine finds the chip and offset of the named line.
//
// Returns an error if the line cannot be found.
func FindLine(lname string) (string, int, error) {
	c, o, err := findLine(lname)
	if err != nil {
		return "", 0, err
	}
	c.Close()
	return c.Name, o, nil
}

// NewChip opens a GPIO character device.
func NewChip(name string, options ...ChipOption) (*Chip, error) {
	path := nameToPath(name)
	err := IsChip(path)
	if err != nil {
		return nil, err
	}
	co := ChipOptions{
		consumer: fmt.Sprintf("gpiod-%d", os.Getpid()),
	}
	for _, option := range options {
		option.applyChipOption(&co)
	}
	f, err := os.OpenFile(path, unix.O_CLOEXEC, unix.O_RDONLY)
	if err != nil {
		// only happens if device removed/locked since IsChip call.
		return nil, err
	}
	ci, err := uapi.GetChipInfo(f.Fd())
	if err != nil {
		// only occurs if IsChip was wrong?
		f.Close()
		return nil, err
	}
	c := Chip{
		f:        f,
		Name:     uapi.BytesToString(ci.Name[:]),
		Label:    uapi.BytesToString(ci.Label[:]),
		lines:    int(ci.Lines),
		consumer: co.consumer,
	}
	if len(c.Label) == 0 {
		c.Label = "unknown"
	}
	return &c, nil
}

// Close releases the Chip.
//
// It does not release any lines which may be requested - they must be closed
// independently.
func (c *Chip) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	c.closed = true
	return c.f.Close()
}

// FindLine returns the offset of the named line, or an error if not found.
func (c *Chip) FindLine(name string) (int, error) {
	for o := 0; o < c.lines; o++ {
		inf, err := c.LineInfo(o)
		if err != nil {
			return 0, err
		}
		if inf.Name == name {
			return o, nil
		}
	}
	return 0, ErrLineNotFound
}

// FindLines returns the offsets of the named lines, or an error unless all are
// found.
func (c *Chip) FindLines(names ...string) ([]int, error) {
	ll := make([]int, len(names))
	for i, name := range names {
		o, err := c.FindLine(name)
		if err != nil {
			return nil, err
		}
		ll[i] = o
	}
	return ll, nil
}

// LineInfo returns the publically available information on the line.
//
// This is always available and does not require requesting the line.
func (c *Chip) LineInfo(offset int) (LineInfo, error) {
	if offset < 0 || offset >= c.lines {
		return LineInfo{}, ErrInvalidOffset
	}
	li, err := uapi.GetLineInfo(c.f.Fd(), offset)
	if err != nil {
		return LineInfo{}, err
	}
	return LineInfo{
		Offset:     offset,
		Name:       uapi.BytesToString(li.Name[:]),
		Consumer:   uapi.BytesToString(li.Consumer[:]),
		Requested:  li.Flags.IsRequested(),
		IsOut:      li.Flags.IsOut(),
		ActiveLow:  li.Flags.IsActiveLow(),
		OpenDrain:  li.Flags.IsOpenDrain(),
		OpenSource: li.Flags.IsOpenSource(),
	}, nil
}

// Lines returns the number of lines that exist on the GPIO chip.
func (c *Chip) Lines() int {
	return c.lines
}

// RequestLine requests control of a single line on the chip.
//
// If granted, control is maintained until either the Line or Chip are closed.
func (c *Chip) RequestLine(offset int, options ...LineOption) (*Line, error) {
	ll, err := c.RequestLines([]int{offset}, options...)
	if err != nil {
		return nil, err
	}
	l := Line{baseLine{
		offsets: ll.offsets,
		vfd:     ll.vfd,
		canset:  ll.canset,
		chip:    c.Name,
		info:    ll.info,
		w:       ll.w,
	}}
	return &l, nil
}

// RequestLines requests control of a collection of lines on the chip.
func (c *Chip) RequestLines(offsets []int, options ...LineOption) (*Lines, error) {
	for _, o := range offsets {
		if o < 0 || o >= c.lines {
			return nil, ErrInvalidOffset
		}
	}
	lo := LineOptions{
		consumer: c.consumer,
	}
	for _, option := range options {
		option.applyLineOption(&lo)
	}
	ll := Lines{baseLine{
		offsets: append([]int(nil), offsets...),
		canset:  lo.HandleFlags.IsOutput(),
		chip:    c.Name,
		info:    make([]LineInfo, len(offsets)),
	}}
	var (
		fd  uintptr
		w   *watcher
		err error
	)
	if lo.eh != nil {
		fd, w, err = c.getEventRequest(ll.offsets, lo)
	} else {
		fd, err = c.getHandleRequest(ll.offsets, lo)
	}
	if err != nil {
		return nil, err
	}
	ll.vfd = fd
	ll.w = w
	for i, o := range offsets {
		inf, err := c.LineInfo(o)
		if err != nil {
			// in case of a race with Chip.Close
			ll.Close()
			return nil, err
		}
		ll.info[i] = inf
	}
	return &ll, nil
}

func (c *Chip) getEventRequest(offsets []int, lo LineOptions) (uintptr, *watcher, error) {
	var vfd uintptr
	fds := make(map[int]int)
	for i, o := range offsets {
		er := uapi.EventRequest{
			Offset:      uint32(o),
			HandleFlags: lo.HandleFlags,
			EventFlags:  lo.EventFlags,
		}
		copy(er.Consumer[:], lo.consumer)
		err := uapi.GetLineEvent(c.f.Fd(), &er)
		if err != nil {
			return 0, nil, err
		}
		fd := uintptr(er.Fd)
		if i == 0 {
			vfd = fd
		}
		fds[int(fd)] = o
	}
	w, err := newWatcher(fds, lo.eh)
	if err != nil {
		for fd := range fds {
			unix.Close(fd)
		}
		return 0, nil, err
	}
	return vfd, w, nil
}

func (c *Chip) getHandleRequest(offsets []int, lo LineOptions) (uintptr, error) {
	hr := uapi.HandleRequest{
		Lines: uint32(len(offsets)),
		Flags: lo.HandleFlags,
	}
	copy(hr.Consumer[:], lo.consumer)
	//copy(hr.Offsets[:], ll.offsets) - with cast
	for i, o := range offsets {
		hr.Offsets[i] = uint32(o)
	}
	//copy(hr.DefaultValues[:], lo.DefaultValues) - with cast
	for i, v := range lo.DefaultValues {
		hr.DefaultValues[i] = uint8(v)
	}
	err := uapi.GetLineHandle(c.f.Fd(), &hr)
	if err != nil {
		return 0, err
	}
	return uintptr(hr.Fd), nil
}

type baseLine struct {
	offsets []int
	vfd     uintptr
	canset  bool
	chip    string
	info    []LineInfo
	mu      sync.Mutex
	closed  bool
	w       *watcher
}

// Chip returns the name of the chip from which the line was requested.
func (l *baseLine) Chip() string {
	return l.chip
}

// Close releases all resources held by the requested line.
func (l *baseLine) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	l.closed = true
	if l.w != nil {
		l.w.close()
	} else {
		unix.Close(int(l.vfd))
	}
	return nil
}

// Line represents a single requested line.
type Line struct {
	baseLine
}

// Offset returns the offset of the line within the chip.
func (l *Line) Offset() int {
	return l.offsets[0]
}

// Info returns the information about the line.
//
// The line info is immutable for the lifetime of the line request.
//
// This is a snapshot of the info taken when the line was requested and provides
// a convenient method to check the options requested on the line.
func (l *Line) Info() LineInfo {
	return l.info[0]
}

// Value returns the current value (active state) of the line.
func (l *Line) Value() (int, error) {
	var values uapi.HandleData
	err := uapi.GetLineValues(l.vfd, &values)
	return int(values[0]), err
}

// SetValue sets the current active state of the line.
//
// Only valid for output lines.
func (l *Line) SetValue(value int) error {
	if l.canset == false {
		return ErrPermissionDenied
	}
	var values uapi.HandleData
	values[0] = uint8(value)
	return uapi.SetLineValues(l.vfd, values)
}

// Lines represents a collection of requested lines.
type Lines struct {
	baseLine
}

// Offsets returns the offsets of the lines within the chip.
func (l *Lines) Offsets() []int {
	return l.offsets
}

// Info returns the information about the lines.
//
// The line info is immuatble for the lifetime of the lines request.
//
// This is a snapshot of the info taken when the lines were requested and provides a
// convenient method to check the options requested on the lines.
func (l *Lines) Info() []LineInfo {
	return l.info
}

// Values returns the current values (active state) of the collection of
// lines.
func (l *Lines) Values() ([]int, error) {
	var values uapi.HandleData
	err := uapi.GetLineValues(l.vfd, &values)
	if err != nil {
		return nil, err
	}
	vv := make([]int, len(l.offsets))
	for i := 0; i < len(l.offsets); i++ {
		vv[i] = int(values[i])
	}
	return vv, nil
}

// SetValues sets the current active state of the collection of lines.
//
// Only valid for output lines.
//
// All lines in the set are set at once.  If insufficient values are provided
// then the remaining lines are set to inactive.
func (l *Lines) SetValues(values []int) error {
	if l.canset == false {
		return ErrPermissionDenied
	}
	if len(values) > len(l.offsets) {
		return ErrInvalidOffset
	}
	var vv uapi.HandleData
	for i, v := range values {
		vv[i] = uint8(v)
	}
	return uapi.SetLineValues(l.vfd, vv)
}

// LineEventType indicates the type of change to the line active state.
//
// Note that for active low lines a low line level results in a high active
// state.
type LineEventType int

const (
	_ LineEventType = iota
	// LineEventRisingEdge indicates an inactive to active event.
	LineEventRisingEdge

	// LineEventFallingEdge indicates an active to inactive event.
	LineEventFallingEdge
)

// LineEvent represents a change in the state of a monitored line.
type LineEvent struct {
	// The line offset within the GPIO chip.
	Offset int

	// Timestamp is the time the event was detected.
	// This is the Unix epoch - nsec since Jan 1 1970.
	Timestamp time.Duration

	// The type of state change event this structure represents.
	Type LineEventType
}

// IsChip checks if the named device is an accessible GPIO character device.
//
// Returns an error if not.
func IsChip(name string) error {
	path := nameToPath(name)
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return ErrNotCharacterDevice
	}
	sysfspath := fmt.Sprintf("/sys/bus/gpio/devices/%s/dev", fi.Name())
	if err = unix.Access(sysfspath, unix.R_OK); err != nil {
		return ErrNotCharacterDevice
	}
	sysfsf, err := os.Open(sysfspath)
	if err != nil {
		return ErrNotCharacterDevice
	}
	var sysfsdev [16]byte
	n, err := sysfsf.Read(sysfsdev[:])
	sysfsf.Close()
	if err != nil || n <= 0 {
		return ErrNotCharacterDevice
	}
	var stat unix.Stat_t
	if err = unix.Lstat(path, &stat); err != nil {
		return err
	}
	devstr := fmt.Sprintf("%d:%d", unix.Major(stat.Rdev), unix.Minor(stat.Rdev))
	sysstr := string(sysfsdev[:n-1])
	if devstr != sysstr {
		return ErrNotCharacterDevice
	}
	return nil
}

// chipNames returns the name of potential gpiochips.
//
// Does not open them or check if they are valid.
func chipNames() []string {
	ee, err := ioutil.ReadDir("/dev")
	if err != nil {
		return nil
	}
	cc := []string(nil)
	for _, e := range ee {
		name := e.Name()
		if strings.HasPrefix(name, "gpiochip") {
			cc = append(cc, name)
		}
	}
	return cc
}

// helper that finds the chip and offset corresponding to a named line.
//
// If found returns the chip and offset, else an error.
func findLine(lname string) (*Chip, int, error) {
	for _, name := range chipNames() {
		c, err := NewChip(name)
		if err != nil {
			continue
		}
		o, err := c.FindLine(lname)
		if err == nil {
			return c, o, nil
		}
	}
	return nil, 0, ErrLineNotFound
}

func nameToPath(name string) string {
	if strings.HasPrefix(name, "/dev/") {
		return name
	}
	return "/dev/" + name
}

var (
	// ErrClosed indicates the chip or line has already been closed.
	ErrClosed = errors.New("already closed")

	// ErrInvalidOffset indicates a line offset is invalid.
	ErrInvalidOffset = errors.New("invalid offset")

	// ErrNotCharacterDevice indicates the device is not a character device.
	ErrNotCharacterDevice = errors.New("not a character device")

	// ErrLineNotFound indicates the line was not found.
	ErrLineNotFound = errors.New("line not found")

	// ErrPermissionDenied indicates caller does not have required permissions
	// for the operation.
	ErrPermissionDenied = errors.New("permission denied")
)
