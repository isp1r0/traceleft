package probe

import (
	"bytes"
	"crypto/sha512"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"github.com/hashicorp/golang-lru"
	elflib "github.com/iovisor/gobpf/elf"
	"github.com/iovisor/gobpf/pkg/bpffs"
)

type Probe struct {
	module        *elflib.Module
	handlerCache  *lru.Cache                  // hash -> *Handler
	pidToHandlers map[int]map[string]struct{} // pid -> syscalls handled (42 -> ["handle_read": struct{}{}, "handle_write": struct{}{}])
}

func evictHandler(key interface{}, value interface{}) {
	if h, ok := value.(*Handler); ok {
		h.Close()
	}
}

type Handler struct {
	module *elflib.Module

	id string // hash of the ELF object containing the handlers

	name string // name of the syscall handler. Example: "handle_read"

	fd    int // file descriptor of the kprobe handler bpf program
	fdRet int // file descriptor of the kretprobe handler bpf program
}

func sha512hex(d []byte) string {
	return fmt.Sprintf("%x", sha512.Sum512(d))
}

func newHandler(elfBPF []byte) (*Handler, error) {
	rd := bytes.NewReader(elfBPF)
	handlerBPF := elflib.NewModuleFromReader(rd)

	// perf map is initialized and polled from global object
	elfSectionParams := map[string]elflib.SectionParams{
		"maps/events": {
			SkipPerfMapInitialization: true,
		},
		"maps/file_events": {
			SkipPerfMapInitialization: true,
		},
	}

	if err := handlerBPF.Load(elfSectionParams); err != nil {
		return nil, fmt.Errorf("error loading handler: %v", err)
	}

	var fd, fdRet int
	var name, nameRet string
	for kp := range handlerBPF.IterKprobes() {
		if strings.HasPrefix(kp.Name, "kprobe/") {
			if name != "" {
				return nil, fmt.Errorf("malformed ELF file, it has more than one kprobe handler")
			}
			fd = kp.Fd()
			name = strings.TrimPrefix(kp.Name, "kprobe/")
		} else if strings.HasPrefix(kp.Name, "kretprobe/") {
			if nameRet != "" {
				return nil, fmt.Errorf("malformed ELF file, it has more than one kretprobe handler")
			}
			fdRet = kp.Fd()
			nameRet = strings.TrimPrefix(kp.Name, "kretprobe/")
		}
	}

	if name == "" || nameRet == "" {
		return nil, fmt.Errorf("malformed ELF file, it should contain both a kprobe and a kretprobe handler")
	}
	if strings.Compare(name, nameRet) != 0 {
		return nil, fmt.Errorf("malformed ELF file, both kprobe and kretprobe handlers should have the same name")
	}

	return &Handler{
		module: handlerBPF,
		name:   name,
		fd:     fd,
		fdRet:  fdRet,
	}, nil
}

func generateProgArrayNames(name string) (progArrayName string, progArrayNameRet string) {
	progArrayName = fmt.Sprintf("%s_progs", name)
	progArrayNameRet = fmt.Sprintf("%s_progs_ret", name)
	return
}

func (probe *Probe) registerHandler(programID uint64, pid int, handler *Handler) error {
	progArrayName, progArrayNameRet := generateProgArrayNames(handler.name)

	progTable := probe.module.Map(progArrayName)
	if progTable == nil {
		return fmt.Errorf("%q doesn't exist", progArrayName)
	}
	progTableRet := probe.module.Map(progArrayNameRet)
	if progTableRet == nil {
		return fmt.Errorf("%q doesn't exist", progArrayNameRet)
	}

	var fd, fdRet int = handler.fd, handler.fdRet
	if err := probe.module.UpdateElement(progTable, unsafe.Pointer(&pid), unsafe.Pointer(&fd), 0); err != nil {
		return fmt.Errorf("error updating %q: %v", progTable.Name, err)
	}
	if err := probe.module.UpdateElement(progTableRet, unsafe.Pointer(&pid), unsafe.Pointer(&fdRet), 0); err != nil {
		return fmt.Errorf("error updating %q: %v", progTableRet.Name, err)
	}

	progIDTable := probe.module.Map("program_id_per_pid")
	if progIDTable == nil {
		return fmt.Errorf("program_id_per_pid doesn't exist")
	}
	if err := probe.module.UpdateElement(progIDTable, unsafe.Pointer(&pid), unsafe.Pointer(&programID), 0); err != nil {
		return fmt.Errorf("error updating the program id table: %v", err)
	}

	pidsToWatchName := "file_events_pids_to_watch"
	watchMap := probe.module.Map(pidsToWatchName)
	if watchMap == nil {
		return fmt.Errorf("%q doesn't exist", pidsToWatchName)
	}

	var one uint32 = 1
	if err := probe.module.UpdateElement(watchMap, unsafe.Pointer(&pid), unsafe.Pointer(&one), 0); err != nil {
		return fmt.Errorf("error updating %q: %v", watchMap.Name, err)
	}

	if _, ok := probe.pidToHandlers[pid]; !ok {
		probe.pidToHandlers[pid] = make(map[string]struct{})
	}

	probe.pidToHandlers[pid][handler.name] = struct{}{}

	return nil
}

func (probe *Probe) RegisterHandlerById(programID uint64, pid int, hash string) error {
	val, ok := probe.handlerCache.Get(hash)
	if !ok {
		return ErrNotInCache
	}

	handler, ok := val.(*Handler)
	if !ok {
		return fmt.Errorf("invalid type")
	}

	return probe.registerHandler(programID, pid, handler)
}

func (probe *Probe) getHandler(elfBPF []byte) (handler *Handler, err error) {
	id := sha512hex(elfBPF)
	val, ok := probe.handlerCache.Get(id)
	if !ok {
		handler, err = newHandler(elfBPF)
		if err != nil {
			return
		}

		probe.handlerCache.Add(id, handler)

		return
	}

	handler, ok = val.(*Handler)
	if !ok {
		return nil, fmt.Errorf("invalid type")
	}

	return
}

func (probe *Probe) RegisterHandler(programID uint64, pid int, elfBPF []byte) error {
	handler, err := probe.getHandler(elfBPF)
	if err != nil {
		return err
	}

	return probe.registerHandler(programID, pid, handler)
}

func (probe *Probe) unregisterHandler(programID uint64, pid int, handlerName string) error {
	progArrayName, progArrayNameRet := generateProgArrayNames(handlerName)
	progTable := probe.module.Map(progArrayName)
	if progTable == nil {
		return fmt.Errorf("%q doesn't exist", progArrayName)
	}
	progTableRet := probe.module.Map(progArrayNameRet)
	if progTableRet == nil {
		return fmt.Errorf("%q doesn't exist", progArrayNameRet)
	}
	progIDTable := probe.module.Map("program_id_per_pid")
	if progIDTable == nil {
		return fmt.Errorf("program_id_per_pid doesn't exist")
	}

	if err := probe.module.DeleteElement(progTable, unsafe.Pointer(&pid)); err != nil {
		return fmt.Errorf("error deleting %q: %v", progTable.Name, err)
	}
	if err := probe.module.DeleteElement(progTableRet, unsafe.Pointer(&pid)); err != nil {
		return fmt.Errorf("error deleting %q: %v", progTableRet.Name, err)
	}
	if err := probe.module.DeleteElement(progIDTable, unsafe.Pointer(&pid)); err != nil {
		return fmt.Errorf("error deleting program id table: %v", err)
	}
	delete(probe.pidToHandlers, pid)

	return nil
}

func (probe *Probe) UnregisterHandler(programID uint64, pid int) error {
	for handlerName := range probe.pidToHandlers[pid] {
		if err := probe.unregisterHandler(programID, pid, handlerName); err != nil {
			return err
		}
	}

	return nil
}

func (probe *Probe) Close() error {
	options := map[string]elflib.CloseOptions{
		"maps/events": {
			Unpin: true,
		},
	}
	return probe.module.CloseExt(options)
}

func (probe *Probe) BPFModule() *elflib.Module {
	return probe.module
}

func (handler *Handler) Id() string {
	return handler.id
}

func (handler *Handler) Close() error {
	return handler.module.Close()
}

func New(cacheSize int) (*Probe, error) {
	if err := bpffs.Mount(); err != nil {
		return nil, err
	}

	buf, err := Asset("trace_events.bpf")
	if err != nil {
		return nil, fmt.Errorf("couldn't find global BPF program asset: %v", err)
	}
	reader := bytes.NewReader(buf)
	globalBPF := elflib.NewModuleFromReader(reader)

	if err := globalBPF.Load(nil); err != nil {
		return nil, fmt.Errorf("error loading global BPF: %v", err)
	}

	untrackedTable := globalBPF.Map("untracked_pids")
	if untrackedTable == nil {
		return nil, fmt.Errorf("%q doesn't exist", "untracked_pids")
	}

	probePid := uint32(os.Getpid())
	var zero uint8 = 0
	if err := globalBPF.UpdateElement(untrackedTable, unsafe.Pointer(&probePid), unsafe.Pointer(&zero), 0); err != nil {
		return nil, fmt.Errorf("error updating %q: %v", untrackedTable.Name, err)
	}

	// TODO choose something here
	if err := globalBPF.EnableKprobes(16); err != nil {
		return nil, err
	}

	cache, err := lru.NewWithEvict(cacheSize, evictHandler)
	if err != nil {
		return nil, err
	}

	return &Probe{
		module:        globalBPF,
		handlerCache:  cache,
		pidToHandlers: make(map[int]map[string]struct{}),
	}, nil
}
