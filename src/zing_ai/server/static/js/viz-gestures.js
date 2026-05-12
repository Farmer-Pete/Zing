/* viz-gestures.js — pan/zoom math for the plan-detail viewer.
 *
 * Mutates Datastar signals (pan, scale) via closures the template wires
 * into data-on:wheel/pointerdown/move/up. Never fetches, never mutates
 * the DOM directly.
 *
 * IMPORTANT: do NOT setPointerCapture on pointerdown — capturing
 * immediately re-targets click events to the stage and prevents them
 * from reaching cards. Capture only on drag promotion (after 6 px of
 * movement) or pinch start (second pointer down).
 */

(function () {
    "use strict";

    var SCALE_MIN = 0.06;
    var SCALE_MAX = 3.5;
    var DRAG_THRESHOLD_PX = 6;

    var state = {
        // pointerId -> { x, y, downX, downY, captured }
        pointers: new Map(),
        // 'idle' | 'drag' | 'pinch'
        gesture: "idle",
        // for pinch: distance + midpoint at gesture start
        pinch: null,
    };

    function _ensureCaptured(evt) {
        var rec = state.pointers.get(evt.pointerId);
        if (rec && !rec.captured) {
            try {
                evt.currentTarget.setPointerCapture(evt.pointerId);
                rec.captured = true;
            } catch (_e) {
                /* element may not support capture; that's fine */
            }
        }
    }

    function _midpoint(a, b) {
        return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
    }

    function _distance(a, b) {
        var dx = b.x - a.x;
        var dy = b.y - a.y;
        return Math.hypot(dx, dy);
    }

    function _stagePoint(evt) {
        var r = evt.currentTarget.getBoundingClientRect();
        return { x: evt.clientX - r.left, y: evt.clientY - r.top };
    }

    function _clampScale(s) {
        return Math.max(SCALE_MIN, Math.min(SCALE_MAX, s));
    }

    window.zingViz = {
        // Diagnostic hook only — tests can read these without poking internals.
        _state: state,

        onWheel: function (evt, panSig, scaleSig) {
            if (evt.ctrlKey) {
                // Pinch-zoom (trackpad pinch arrives as wheel + ctrlKey).
                var factor = Math.exp(-evt.deltaY * 0.012);
                var anchor = _stagePoint(evt);
                var cxBefore = (anchor.x - panSig.x) / scaleSig;
                var cyBefore = (anchor.y - panSig.y) / scaleSig;
                var newScale = _clampScale(scaleSig * factor);
                // Re-anchor pan so the cursor point stays put under the new scale.
                panSig.x = anchor.x - cxBefore * newScale;
                panSig.y = anchor.y - cyBefore * newScale;
                // Datastar reactivity: replace the scalar by reassigning into the
                // proxied object. We can't reassign the local `scaleSig` ref, so
                // expose it via a setter property the template binds.
                // The template uses `$scale` directly, so we mutate it by
                // re-emitting through an event. Simpler: set via a global helper.
                window.zingViz._setScale(newScale);
            } else {
                // Two-finger trackpad scroll = 2D pan.
                panSig.x -= evt.deltaX;
                panSig.y -= evt.deltaY;
            }
        },

        // Set by the viewer after data-init via a tiny inline binding;
        // serves as the bridge for scale because pan is mutated by reference
        // (object) but scale is a scalar.
        _setScale: function (_v) {
            /* overridden at runtime by the template */
        },

        onPointerDown: function (evt) {
            state.pointers.set(evt.pointerId, {
                x: evt.clientX,
                y: evt.clientY,
                downX: evt.clientX,
                downY: evt.clientY,
                captured: false,
            });

            if (state.pointers.size === 2 && state.gesture !== "pinch") {
                // Promote to pinch — capture both pointers, snapshot start state.
                state.gesture = "pinch";
                _ensureCaptured(evt);
                var pts = Array.from(state.pointers.values());
                state.pinch = {
                    startDist: _distance(pts[0], pts[1]),
                    startMid: _midpoint(pts[0], pts[1]),
                };
            }
            // Single-pointer down stays in 'idle' until movement exceeds threshold.
        },

        onPointerMove: function (evt, panSig, scaleSig) {
            var rec = state.pointers.get(evt.pointerId);
            if (!rec) return;
            rec.x = evt.clientX;
            rec.y = evt.clientY;

            if (state.gesture === "pinch" && state.pointers.size >= 2) {
                var pts = Array.from(state.pointers.values());
                var dist = _distance(pts[0], pts[1]);
                var mid = _midpoint(pts[0], pts[1]);
                var factor = dist / state.pinch.startDist;
                var newScale = _clampScale(scaleSig * factor);
                // Anchor at pinch midpoint (in stage coords).
                var r = evt.currentTarget.getBoundingClientRect();
                var anchor = { x: mid.x - r.left, y: mid.y - r.top };
                var cxBefore = (anchor.x - panSig.x) / scaleSig;
                var cyBefore = (anchor.y - panSig.y) / scaleSig;
                panSig.x = anchor.x - cxBefore * newScale;
                panSig.y = anchor.y - cyBefore * newScale;
                window.zingViz._setScale(newScale);
                state.pinch.startDist = dist; // continuous tracking
                return;
            }

            // Single-pointer drag promotion.
            var dx = evt.clientX - rec.downX;
            var dy = evt.clientY - rec.downY;
            if (state.gesture === "idle" && Math.hypot(dx, dy) > DRAG_THRESHOLD_PX) {
                state.gesture = "drag";
                _ensureCaptured(evt);
            }
            if (state.gesture === "drag") {
                // Pan by the per-frame delta.
                panSig.x += evt.movementX || 0;
                panSig.y += evt.movementY || 0;
            }
        },

        onPointerUp: function (evt) {
            var rec = state.pointers.get(evt.pointerId);
            if (rec && rec.captured) {
                try {
                    evt.currentTarget.releasePointerCapture(evt.pointerId);
                } catch (_e) {
                    /* nothing */
                }
            }
            state.pointers.delete(evt.pointerId);
            if (state.pointers.size === 0) {
                state.gesture = "idle";
                state.pinch = null;
            } else if (state.pointers.size === 1) {
                // Dropped one of two pointers — exit pinch but stay alive for drag.
                state.gesture = "idle";
                state.pinch = null;
            }
        },
    };
})();
