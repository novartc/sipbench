#!/usr/bin/env bash

ln -sf /usr/include/bpf/bpf_helper_defs.h bpf_helper_defs.h
ln -sf /usr/include/bpf/bpf_helpers.h bpf_helpers.h
ln -sf /usr/include/bpf/bpf_tracing.h bpf_tracing.h
ln -sf /usr/include/bpf/bpf_core_read.h bpf_core_read.h
ln -sf /usr/include/bpf/bpf_endian.h bpf_endian.h

bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h
