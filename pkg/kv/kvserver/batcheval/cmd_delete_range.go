// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package batcheval

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/batcheval/result"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/spanset"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/errors"
)

func init() {
	RegisterReadWriteCommand(roachpb.DeleteRange, declareKeysDeleteRange, DeleteRange)
}

func declareKeysDeleteRange(
	rs ImmutableRangeState,
	header *roachpb.Header,
	req roachpb.Request,
	latchSpans, lockSpans *spanset.SpanSet,
	maxOffset time.Duration,
) {
	args := req.(*roachpb.DeleteRangeRequest)
	if args.Inline {
		DefaultDeclareKeys(rs, header, req, latchSpans, lockSpans, maxOffset)
	} else {
		DefaultDeclareIsolatedKeys(rs, header, req, latchSpans, lockSpans, maxOffset)
	}

	// When writing range tombstones, we must look for adjacent range tombstones
	// that we merge with or fragment, to update MVCC stats accordingly. But we
	// make sure to stay within the range bounds.
	if args.UseRangeTombstone {
		// NB: The range end key is not available, so this will pessimistically
		// latch up to args.EndKey.Next(). If EndKey falls on the range end key, the
		// span will be tightened during evaluation.
		l, r := rangeTombstonePeekBounds(args.Key, args.EndKey, rs.GetStartKey().AsRawKey(), nil)
		latchSpans.AddMVCC(spanset.SpanReadOnly, roachpb.Span{Key: l, EndKey: r}, header.Timestamp)

		// We need to read the range descriptor to determine the bounds during eval.
		latchSpans.AddNonMVCC(spanset.SpanReadOnly, roachpb.Span{
			Key: keys.RangeDescriptorKey(rs.GetStartKey()),
		})
	}
}

// DeleteRange deletes the range of key/value pairs specified by
// start and end keys.
func DeleteRange(
	ctx context.Context, readWriter storage.ReadWriter, cArgs CommandArgs, resp roachpb.Response,
) (result.Result, error) {
	args := cArgs.Args.(*roachpb.DeleteRangeRequest)
	h := cArgs.Header
	reply := resp.(*roachpb.DeleteRangeResponse)

	// Use experimental MVCC range tombstone if requested.
	if args.UseRangeTombstone {
		if cArgs.Header.Txn != nil {
			return result.Result{}, ErrTransactionUnsupported
		}
		if args.Inline {
			return result.Result{}, errors.AssertionFailedf("Inline can't be used with range tombstones")
		}
		if args.ReturnKeys {
			return result.Result{}, errors.AssertionFailedf(
				"ReturnKeys can't be used with range tombstones")
		}

		desc := cArgs.EvalCtx.Desc()
		leftPeekBound, rightPeekBound := rangeTombstonePeekBounds(
			args.Key, args.EndKey, desc.StartKey.AsRawKey(), desc.EndKey.AsRawKey())
		maxIntents := storage.MaxIntentsPerWriteIntentError.Get(&cArgs.EvalCtx.ClusterSettings().SV)

		err := storage.MVCCDeleteRangeUsingTombstone(ctx, readWriter, cArgs.Stats,
			args.Key, args.EndKey, h.Timestamp, cArgs.Now, leftPeekBound, rightPeekBound, maxIntents)
		return result.Result{}, err
	}

	var timestamp hlc.Timestamp
	if !args.Inline {
		timestamp = h.Timestamp
	}
	// NB: Even if args.ReturnKeys is false, we want to know which intents were
	// written if we're evaluating the DeleteRange for a transaction so that we
	// can update the Result's AcquiredLocks field.
	returnKeys := args.ReturnKeys || h.Txn != nil
	deleted, resumeSpan, num, err := storage.MVCCDeleteRange(
		ctx, readWriter, cArgs.Stats, args.Key, args.EndKey,
		h.MaxSpanRequestKeys, timestamp, cArgs.Now, h.Txn, returnKeys)
	if err == nil && args.ReturnKeys {
		reply.Keys = deleted
	}
	reply.NumKeys = num
	if resumeSpan != nil {
		reply.ResumeSpan = resumeSpan
		reply.ResumeReason = roachpb.RESUME_KEY_LIMIT
	}
	// NB: even if MVCC returns an error, it may still have written an intent
	// into the batch. This allows callers to consume errors like WriteTooOld
	// without re-evaluating the batch. This behavior isn't particularly
	// desirable, but while it remains, we need to assume that an intent could
	// have been written even when an error is returned. This is harmless if the
	// error is not consumed by the caller because the result will be discarded.
	return result.FromAcquiredLocks(h.Txn, deleted...), err
}
