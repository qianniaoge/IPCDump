package collection

import (
    "fmt"
    "bytes"
    "encoding/binary"
    "syscall"
    "github.com/iovisor/gobpf/bcc"
    "github.com/guardicode/ipcdump/internal/bpf"
    "github.com/guardicode/ipcdump/internal/events"
)

const signalIncludes = ""

const signalSource = `
BPF_PERF_OUTPUT(signal_events);

struct __attribute__((packed)) signal_data_t {
    u64 sig;
    // this is too big
    u64 src_pid;  // PID as in the userspace term (i.e. task->tgid in kernel)
    char src_comm[16];
    u64 dst_pid;
    char dst_comm[16];
    u64 timestamp;
};

struct __attribute__((packed)) signal_generate_args_t {
    u16 common_type;
    unsigned char common_flags;
    unsigned char common_preempt_count;
    u32 common_pid;

    u32 sig;
    u32 errno;
    u32 code;
    char comm[16];
    u32 pid;
    u32 group;
    u32 result;
};

int trace_signal_generate(struct signal_generate_args_t *args) {
    if (args->result == 0 && args->code == 0) {
        struct signal_data_t signal = {
            .sig = args->sig,
            .dst_pid = args->pid,
        };
        signal.src_pid = bpf_get_current_pid_tgid() >> 32;
        bpf_get_current_comm(signal.src_comm, sizeof(signal.src_comm));
        signal.timestamp = bpf_ktime_get_ns();
        get_comm_for_pid(signal.dst_pid, signal.dst_comm, sizeof(signal.dst_comm));
        signal_events.perf_submit(args, &signal, sizeof(signal));
    }
    return 0;
}`

type signalIpcEvent struct {
    Sig uint64
    SrcPid uint64
    SrcComm [16]byte
    DstPid uint64
    DstComm [16]byte
    Timestamp uint64
}

func handleSignalEvent(event *signalIpcEvent, commId *CommIdentifier) error {
    signalNum := syscall.Signal(event.Sig)
    e := events.IpcEvent{
        Src: makeIpcEndpoint(commId, event.SrcPid, event.SrcComm),
        Dst: makeIpcEndpoint(commId, event.DstPid, event.DstComm),
        Type: events.IPC_EVENT_SIGNAL,
        Timestamp: TsFromKtime(event.Timestamp),
        Metadata: events.IpcMetadata{
            events.IpcMetadataPair{Name: "num", Value: event.Sig},
            events.IpcMetadataPair{Name: "name", Value: signalNum.String()},
        },
    }
    return events.EmitIpcEvent(e)
}

func InitSignalCollection(bpfBuilder *bpf.BpfBuilder) error {
    if err := bpfBuilder.AddIncludes(signalIncludes); err != nil {
        return err
    }
    bpfBuilder.AddSources(signalSource)
    return nil
}

func CollectSignals(module *bcc.Module, exit <-chan struct{}, commId *CommIdentifier) error {
    perfChannel := make(chan []byte, 32)
    table := bcc.NewTable(module.TableId("signal_events"), module)
    perfMap, err := bcc.InitPerfMap(table, perfChannel, nil)
    if err != nil {
        return err
    }

    perfMap.Start()
    defer perfMap.Stop()

    tracepoint, err := module.LoadTracepoint("trace_signal_generate")
    if err != nil {
        return err
    }

    if err = module.AttachTracepoint("signal:signal_generate", tracepoint); err != nil {
        return err
    }

    for {
        select {
        case perfData := <-perfChannel:
            var event signalIpcEvent
            if err := binary.Read(bytes.NewBuffer(perfData), bcc.GetHostByteOrder(), &event); err != nil {
                return fmt.Errorf("failed to parse signal event: %w", err)
            }
            if err := handleSignalEvent(&event, commId); err != nil {
                return fmt.Errorf("failed to handle signal event: %w", err)
            }

        case <- exit:
            return nil
        }
    }
}
