// SPDX-License-Identifier: GPL-2.0
// awg-mesh: TC ingress program for inter-WG-interface forwarding on masters.
//
// Attached to each WG interface (e.g., wg-client-01). When a packet arrives
// with a destination IP found in fwd_map, redirect it to the target interface
// (e.g., wg-kz-01) via bpf_redirect. This bypasses the full kernel network
// stack for inter-tunnel forwarding.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <bpf/bpf_helpers.h>

// fwd_map: overlay destination IP (network byte order) → target interface index.
// Updated from Go when tunnels are created/destroyed.
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 256);
    __type(key, __be32);    // destination overlay IP
    __type(value, __u32);   // target interface index (ifindex)
} fwd_map SEC(".maps");

// tc_forward: TC classifier program (ingress hook).
// Looks up packet destination IP in fwd_map. If found, redirects to the
// target interface. Otherwise, passes the packet to the normal stack.
SEC("tc")
int tc_forward(struct __sk_buff *skb) {
    // Only handle IPv4
    if (skb->protocol != __constant_htons(ETH_P_IP))
        return TC_ACT_OK;

    // Read destination IP from IPv4 header
    __be32 dst_ip;
    int ret = bpf_skb_load_bytes(skb, ETH_HLEN + offsetof(struct iphdr, daddr),
                                  &dst_ip, sizeof(dst_ip));
    if (ret < 0)
        return TC_ACT_OK;

    // Lookup in forwarding map
    __u32 *target_ifindex = bpf_map_lookup_elem(&fwd_map, &dst_ip);
    if (!target_ifindex)
        return TC_ACT_OK; // Not in map — normal routing

    // Redirect to target interface
    return bpf_redirect(*target_ifindex, 0);
}

char _license[] SEC("license") = "GPL";
