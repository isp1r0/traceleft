package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"unsafe"

	"github.com/ShiftLeftSecurity/traceleft/probe"
	elflib "github.com/iovisor/gobpf/elf"
)

import "C"

var (
	eventMap *string
)

// this has to match the struct in trace_syscalls.c and handlers.
type readEvent struct {
	Timestamp uint64
	Syscall   [64]byte
	Buffer    [256]byte
	Pid       uint32
	Fd        uint32
	Ret       int32
}

func init() {
	eventMap = flag.String("event-map", "", "Comma-separated [PID]:[elf object] pairs")
}

type Tracer struct {
	m        *elflib.Module
	perfMap  *elflib.PerfMap
	stopChan chan struct{}
}

func timestamp(data *[]byte) uint64 {
	var event readEvent
	err := binary.Read(bytes.NewBuffer(*data), binary.LittleEndian, &event)
	if err != nil {
		fmt.Fprintf(os.Stderr, "timestamp() failed to decode received data: %v\n", err)
		return 0
	}

	return uint64(event.Timestamp)
}

func NewTracer(callback func(*[]byte)) (*Tracer, error) {
	globalBPF, err := probe.Load()
	if err != nil {
		return nil, fmt.Errorf("error loading probe: %v", err)
	}

	channel := make(chan []byte)
	perfMap, err := elflib.InitPerfMap(globalBPF, "events", channel, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to init perf map: %v", err)
	}

	perfMap.SetTimestampFunc(timestamp)

	stopChan := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopChan:
				return
			case data := <-channel:
				callback(&data)
			}
		}
	}()

	perfMap.PollStart()

	return &Tracer{
		m:        globalBPF,
		perfMap:  perfMap,
		stopChan: stopChan,
	}, nil
}

func (t *Tracer) Stop() {
	close(t.stopChan)
	t.perfMap.PollStop()
}

func (t *Tracer) BPFModule() *elflib.Module {
	return t.m
}

func handleEvent(data *[]byte) {
	var event readEvent
	err := binary.Read(bytes.NewBuffer(*data), binary.LittleEndian, &event)
	if err != nil {
		fmt.Printf("failed to decode received data: %v\n", err)
		return
	}
	syscall := (*C.char)(unsafe.Pointer(&event.Syscall))
	buffer := (*C.char)(unsafe.Pointer(&event.Buffer))
	len := C.int(0)
	if event.Ret > 0 {
		len = C.int(event.Ret)
	}
	bufferGo := C.GoStringN(buffer, len)
	fmt.Printf("syscall %s pid %d fd %d return value %d buffer %s\n",
		C.GoString(syscall), event.Pid, event.Fd, event.Ret, bufferGo)
}

func registerEvents(bpfModule *elflib.Module, events map[int]string) error {
	for pid, bpfFile := range events {
		elfBPFBytes, err := ioutil.ReadFile(bpfFile)
		if err != nil {
			return fmt.Errorf("error reading %q: %v", bpfFile, err)
		}

		if err := probe.RegisterHandler(bpfModule, []int{pid}, elfBPFBytes); err != nil {
			return fmt.Errorf("error registering handler: %v", err)
		}
	}

	return nil
}

func parseEventMap(eventMap string) (map[int]string, error) {
	events := make(map[int]string)
	eventsParts := strings.Split(eventMap, ",")
	for _, ev := range eventsParts {
		evParts := strings.Split(ev, ":")
		if len(evParts) != 2 {
			return nil, fmt.Errorf("malformed event-map %q", ev)
		}
		pid, err := strconv.Atoi(evParts[0])
		if err != nil {
			return nil, fmt.Errorf("malformed pid %q in event-map", evParts[0])
		}
		ebpfFile := evParts[1]

		events[pid] = ebpfFile
	}

	return events, nil
}

func main() {
	flag.Parse()

	if flag.NFlag() < 1 {
		flag.PrintDefaults()
		os.Exit(0)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, os.Kill)

	tracer, err := NewTracer(handleEvent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	events, err := parseEventMap(*eventMap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	if err := registerEvents(tracer.BPFModule(), events); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	<-sig
	tracer.Stop()
}
