# Design Overview

## Components

### metagenerator

`metagenerator/metagenerator.go` compiles event structures. Currently, these are:

* `battery/event-structs-generated.h`: contains C structs for all known events.
* `tracer/event-structs-generated.go`: contains Go structs and methods for all
  known events.
* `tracer/event-structs-generated.{proto,pb.go}`: contains Protobuf definitions
  for all known events.

That is, by going through the format files in
`/sys/kernel/debug/tracing/events/syscalls/sys_enter_*` for a list of considered
syscalls.

### generator

`generator/generator.go` autogenerates eBPF handlers for syscalls according to
[config.json](../config.json).

### battery

`battery/` contains a collection of handler eBPF programs. The handlers
are partly handwritten, partly autogenerated (see [generator/](../generator/)).

### probe

`probe` is responsible for registering and unregistering eBPF handlers and
includes a lru handler cache.

### tracer

`tracer` loads a the probe, starts polling the `events` perf map and
calls the `callback` for each received event.

### metrics

`metrics/` contains experimental aggregator code.

## Architecture

![traceleft-architecture](traceleft-architecture.png)

traceleft adds k{ret,}probes for all traced syscalls and kernel functions (see
`bpf/trace_syscalls.c` for a list). The only thing the trace probes do is tail
calling handler probes (or return 0 - i.e. do nothing - if no probe was set).

Examples:

```
// kprobe
bpf_tail_call(ctx, (void *)&handle_open_progs, pid >> 32); // exec process-specific handler, if set or
bpf_tail_call(ctx, (void *)&handle_open_progs, 0);         // exec default handler, if set or
return 0;                                                  // do nothing

// kretprobe, same principle
bpf_tail_call(ctx, (void *)&handle_open_progs_ret, pid >> 32);
bpf_tail_call(ctx, (void *)&handle_open_progs_ret, 0);
return 0;
```

The handler maps follow the scheme `handle_NAME_progs{,_ret}` where
`NAME` is the name of the traced function (w/o `[Ss]y[Ss]_` prefix in
the case of syscalls).

traceleft provides a single map of type `BPF_MAP_TYPE_PROG_ARRAY` which
handlers must use to send events. All events start with a common section

```
typedef struct {
	uint64_t timestamp;
	uint64_t program_id;
	int64_t  tgid;
	int64_t  ret;
	char     name[64];
	uint64_t hash;
} common_event_t;
```

to enable the tracer to dispatch events. Specific fields follow after.

The Probe (see `probe/probe.go`; responsible for loading process-specific as
well as default handler probes) expects handler probes to follow the scheme
`kprobe/handle_NAME` and `kretprobe/handle_NAME`, i.e. the name of a
k{,ret}probe defines which handler map to update.

Handler probes can be loaded for a specific pid or as default handler (pid == 0).