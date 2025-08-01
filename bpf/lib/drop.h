/* SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause) */
/* Copyright Authors of Cilium */

/*
 * Drop & error notification via perf event ring buffer
 *
 * API:
 * int send_drop_notify(ctx, src, dst, dst_id, reason, exitcode, enum metric_dir direction)
 * int send_drop_notify_error(ctx, error, exitcode, enum metric_dir direction)
 *
 * If DROP_NOTIFY is not defined, the API will be compiled in as a NOP.
 */

#pragma once

#include "dbg.h"
#include "events.h"
#include "common.h"
#include "utils.h"
#include "metrics.h"
#include "ratelimit.h"
#include "tailcall.h"
#include "classifiers.h"

#define NOTIFY_DROP_VER 2

struct drop_notify {
	NOTIFY_CAPTURE_HDR
	__u32		src_label;
	__u32		dst_label;
	__u32		dst_id; /* 0 for egress */
	__u16		line;
	__u8		file;
	__s8		ext_error;
	__u32		ifindex;
	__u8		flags; /* __u8 instead of cls_flags_t so that it will error
				* when cls_flags_t grows (either borrow bits from pad2 or
				* move to flags_lower/flags_upper).
				*/
	__u8		pad2[3];
};

#ifdef DROP_NOTIFY
/*
 * We pass information in the meta area as follows:
 *
 *     +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
 *     |                         Source Label                          |
 *     +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
 *     |                       Destination Label                       |
 *     +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
 *     |  Error Code   | Extended Error|            Unused             |
 *     +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
 *     |             Designated Destination Endpoint ID                |
 *     +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
 *     |   Exit Code   |  Source File  |         Source Line           |
 *     +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
 *
 */

__declare_tail(CILIUM_CALL_DROP_NOTIFY)
int tail_drop_notify(struct __ctx_buff *ctx)
{
	/* Mask needed to calm verifier. */
	__u32 error = ctx_load_meta(ctx, 2) & 0xFFFFFFFF;
	__u64 ctx_len = ctx_full_len(ctx);
	__u64 cap_len;
	__u32 meta4 = ctx_load_meta(ctx, 4);
	__u16 line = (__u16)(meta4 >> 16);
	__u8 file = (__u8)(meta4 >> 8);
	__u8 exitcode = (__u8)meta4;
	cls_flags_t flags = CLS_FLAG_NONE;
	struct ratelimit_key rkey = {
		.usage = RATELIMIT_USAGE_EVENTS_MAP,
	};
	struct ratelimit_settings settings = {
		.topup_interval_ns = NSEC_PER_SEC,
	};
	struct drop_notify msg;

	if (EVENTS_MAP_RATE_LIMIT > 0) {
		settings.bucket_size = EVENTS_MAP_BURST_LIMIT;
		settings.tokens_per_topup = EVENTS_MAP_RATE_LIMIT;
		if (!ratelimit_check_and_take(&rkey, &settings))
			return exitcode;
	}

	flags = ctx_classify(ctx, 0, TRACE_POINT_UNKNOWN);
	cap_len = compute_capture_len(ctx, 0, flags, TRACE_POINT_UNKNOWN);

	msg = (typeof(msg)) {
		__notify_common_hdr(CILIUM_NOTIFY_DROP, (__u8)error),
		__notify_pktcap_hdr((__u32)ctx_len, (__u16)cap_len, NOTIFY_DROP_VER),
		.src_label	= ctx_load_meta(ctx, 0),
		.dst_label	= ctx_load_meta(ctx, 1),
		.dst_id		= ctx_load_meta(ctx, 3),
		.line           = line,
		.file           = file,
		.ext_error      = (__s8)(__u8)(error >> 8),
		.ifindex        = ctx_get_ifindex(ctx),
		.flags          = flags,
	};

	ctx_event_output(ctx, &cilium_events,
			 (cap_len << 32) | BPF_F_CURRENT_CPU,
			 &msg, sizeof(msg));

	return exitcode;
}

/**
 * send_drop_notify
 * @ctx:	socket buffer
 * @src:	source identity
 * @dst:	destination identity
 * @dst_id:	designated destination endpoint ID, if ingress, otherwise 0
 * @reason:	Reason for drop
 * @exitcode:	error code to return to the kernel
 *
 * Generate a notification to indicate a packet was dropped.
 *
 * NOTE: This is terminal function and will cause the BPF program to exit
 */
static __always_inline int
_send_drop_notify(__u8 file, __u16 line, struct __ctx_buff *ctx,
		  __u32 src, __u32 dst, __u32 dst_id,
		  __u32 reason, __u32 exitcode, enum metric_dir direction)
{
	int ret __maybe_unused;

	/* These fields should be constants and fit (together) in 32 bits */
	if (!__builtin_constant_p(exitcode) || exitcode > 0xff ||
	    !__builtin_constant_p(file) || file > 0xff ||
	    !__builtin_constant_p(line) || line > 0xffff)
		__throw_build_bug();

	/* Non-zero 'dst_id' is only to be used for ingress. */
	if (dst_id != 0 && (!__builtin_constant_p(direction) || direction != METRIC_INGRESS))
		__throw_build_bug();

	ctx_store_meta(ctx, 0, src);
	ctx_store_meta(ctx, 1, dst);
	ctx_store_meta(ctx, 2, reason);
	ctx_store_meta(ctx, 3, dst_id);
	ctx_store_meta(ctx, 4, exitcode | file << 8 | line << 16);

	_update_metrics(ctx_full_len(ctx), direction, (__u8)reason, line, file);
	ret = tail_call_internal(ctx, CILIUM_CALL_DROP_NOTIFY, NULL);
	/* ignore the returned error, use caller-provided exitcode */

	return exitcode;
}
#else
static __always_inline
int _send_drop_notify(__u8 file __maybe_unused, __u16 line __maybe_unused,
		      struct __ctx_buff *ctx, __u32 src __maybe_unused,
		      __u32 dst __maybe_unused, __u32 dst_id __maybe_unused,
		      __u32 reason, __u32 exitcode, enum metric_dir direction)
{
	_update_metrics(ctx_full_len(ctx), direction, (__u8)reason, line, file);
	return exitcode;
}
#endif /* DROP_NOTIFY */

/*
 * The following macros are required in order to pass source file/line
 * information. The *_ext versions take an additional parameter ext_err
 * which can be used to pass additional information, e.g., this could be an
 * original error returned by fib_lookup (if reason == DROP_NO_FIB).
 */

/*
 * Cilium errors are greater than absolute errno values, so we just pass
 * a positive value here
 */
#define __DROP_REASON(err) ({ \
	typeof(err) __err = (err); \
	(__u8)(__err > 0 ? __err : -__err); \
})

/*
 * We only have 8 bits here to pass either a small positive value or an errno
 * (this can be fixed by changing the layout of struct drop_notify, but for now
 * we can hack this as follows). So we pass a negative errno value as is if it
 * is >= -128, and set it 0 if it is < -128 (which actually shoudn't happen in
 * our case)
 */
#define __DROP_REASON_EXT(err, ext_err) ({ \
	typeof(ext_err) __ext_err = (ext_err); \
	__DROP_REASON(err) | ((__u8)(__ext_err < -128 ? 0 : __ext_err) << 8); \
})

#define send_drop_notify(ctx, src, dst, dst_id, reason, direction) \
	_send_drop_notify(__MAGIC_FILE__, __MAGIC_LINE__, ctx, src, dst, dst_id, \
			  __DROP_REASON(reason), CTX_ACT_DROP, direction)

#define send_drop_notify_error(ctx, src, reason, direction) \
	_send_drop_notify(__MAGIC_FILE__, __MAGIC_LINE__, ctx, src, 0, 0, \
			  __DROP_REASON(reason), CTX_ACT_DROP, direction)

#define send_drop_notify_ext(ctx, src, dst, dst_id, reason, ext_err, direction) \
	_send_drop_notify(__MAGIC_FILE__, __MAGIC_LINE__, ctx, src, dst, dst_id, \
			  __DROP_REASON_EXT(reason, ext_err), CTX_ACT_DROP, direction)

#define send_drop_notify_error_ext(ctx, src, reason, ext_err, direction) \
	_send_drop_notify(__MAGIC_FILE__, __MAGIC_LINE__, ctx, src, 0, 0, \
			  __DROP_REASON_EXT(reason, ext_err), CTX_ACT_DROP, direction)

#define send_drop_notify_error_with_exitcode_ext(ctx, src, reason, ext_err, exitcode, direction) \
	_send_drop_notify(__MAGIC_FILE__, __MAGIC_LINE__, ctx, src, 0, 0, \
			  __DROP_REASON_EXT(reason, ext_err), exitcode, direction)
