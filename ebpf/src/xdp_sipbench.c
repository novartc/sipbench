#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/in.h>
#include <linux/ip.h>
#include <linux/udp.h>

#include "bpf_endian.h"
#include "bpf_helpers.h"


#ifndef memcpy
#define memcpy(dest, src, n) __builtin_memcpy((dest), (src), (n))
#endif


struct rtp_info {
    __u64 bytes;
    __u64 pkts;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, __u64);
    __type(value, struct rtp_info);
    __uint(max_entries, 65535);
} rules SEC(".maps");


SEC("xdp")
int sipbench(struct xdp_md *ctx) {
    void *data_end = (void *) (long) ctx->data_end;
    void *data = (void *) (long) ctx->data;
    struct rtp_info *tnl;
    struct ethhdr *eth = data;
    struct iphdr *iph = (struct iphdr *) (eth + 1);
    struct udphdr *udp = (struct udphdr *) (iph + 1);
    __u16 l4_size;
    __u64 key;


    if ((void *) (eth + 1) > data_end)
        return XDP_DROP;

    if (eth->h_proto != bpf_htons(ETH_P_IP))
        return XDP_PASS;

    if ((void *) (iph + 1) > data_end)
        return XDP_DROP;

    if (iph->ihl != 5) {
        return XDP_DROP;
    }

    if (iph->ttl == 0 || iph->protocol != IPPROTO_UDP) {
        return XDP_PASS;
    }

    if ((void *) (udp + 1) > data_end)
        return XDP_DROP;

    key = udp->dest;
    tnl = bpf_map_lookup_elem(&rules, &key);
    if (!tnl) {
        return XDP_PASS;
    }

    l4_size = bpf_ntohs(udp->len);

    __sync_fetch_and_add(&tnl->pkts, 1);
    __sync_fetch_and_add(&tnl->bytes, l4_size);
    return XDP_DROP;
}

char _license[] SEC("license") = "GPL";
