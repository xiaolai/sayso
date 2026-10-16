// --- matching ---

/// Simulates the units over the transcript positions as bitsets: bit j of the frontier says the units so far can end
/// at token j. A step ANDs the frontier with the positions where an accepted token (or a hyphen chain that joins to
/// it) starts and shifts by its length. Work follows the frontier's actual span, so a short script costs little and
/// an adversarial one costs its span, not a memo of every (unit, position) pair.
func svMatch(_ units: [SvUnit], _ toks: [SvTok]) -> Bool {
    let m = newSvMasks(toks)
    let words = toks.count / 64 + 1
    var frontier = SvSet(words)
    var next = SvSet(words)
    let a = SvSet(words)
    let b = SvSet(words)
    frontier.set(0)
    for unit in units {
        next.reset()
        for seq in unit {
            var cur = frontier
            for (p, alt) in seq.enumerated() {
                let dst = p % 2 == 1 ? b : a
                dst.reset()
                for s in alt {
                    for e in m[s] ?? [] { dst.orShifted(cur, e) }
                }
                cur = dst
                if cur.isEmpty { break }
            }
            next.or(cur)
        }
        swap(&frontier, &next)
        if frontier.isEmpty { return false }
    }
    return frontier.has(toks.count)
}

/// Hyphen chains joined for matching ("e-mail" = "email"); longer chains match part by part.
let svMaxChain = 8

/// The transcript positions where a token string (k = 1) or a hyphen chain of k tokens joining to it starts, stored
/// sparsely by 64-bit word.
final class SvMask {
    let k: Int
    var idx: [Int] = []
    var bits: [UInt64] = []
    init(k: Int) { self.k = k }
}

func newSvMasks(_ toks: [SvTok]) -> [String: [SvMask]] {
    var m: [String: [SvMask]] = [:]
    func add(_ s: String, _ k: Int, _ pos: Int) {
        let e: SvMask
        if let x = m[s]?.first(where: { $0.k == k }) {
            e = x
        } else {
            e = SvMask(k: k)
            m[s, default: []].append(e)
        }
        let w = pos / 64
        if e.idx.last != w {
            e.idx.append(w)
            e.bits.append(0)
        }
        e.bits[e.bits.count - 1] |= 1 << UInt64(pos % 64)
    }
    for (j, t) in toks.enumerated() {
        for v in svMarkReadings(t.text, lead: t.lead, trail: t.trail) { add(v, 1, j) }
        var joined = t.text
        var k = 2
        while k <= svMaxChain && j + k - 1 < toks.count && toks[j + k - 2].link {
            joined += toks[j + k - 1].text
            for v in svMarkReadings(joined, lead: t.lead, trail: toks[j + k - 1].trail) { add(v, k, j) }
            k += 1
        }
    }
    return m
}

/// `s`, then `s` with the runs of marks dropped quotation marks may have been, as apostrophes: the `lead` marks before
/// it, the `trail` marks after it, or both. At most four readings, so the work stays linear in the marks.
func svMarkReadings(_ s: String, lead: Int, trail: Int) -> [String] {
    let before = String(repeating: "'", count: lead)
    let after = String(repeating: "'", count: trail)
    var out = [s]
    if lead > 0 { out.append(before + s) }
    if trail > 0 { out.append(s + after) }
    if lead > 0 && trail > 0 { out.append(before + s + after) }
    return out
}

final class SvSet {
    var w: [UInt64]
    var lo: Int  // words outside lo...hi are zero
    var hi: Int

    init(_ words: Int) {
        w = [UInt64](repeating: 0, count: words)
        lo = words
        hi = -1
    }

    func reset() {
        if lo <= hi {
            for i in lo...hi { w[i] = 0 }
        }
        lo = w.count
        hi = -1
    }

    func put(_ i: Int, _ v: UInt64) {
        if v == 0 { return }
        w[i] |= v
        lo = min(lo, i)
        hi = max(hi, i)
    }

    func set(_ pos: Int) { put(pos / 64, 1 << UInt64(pos % 64)) }
    func has(_ pos: Int) -> Bool { w[pos / 64] & (1 << UInt64(pos % 64)) != 0 }
    var isEmpty: Bool { lo > hi }

    func or(_ src: SvSet) {
        if src.lo > src.hi { return }
        for i in src.lo...src.hi { put(i, src.w[i]) }
    }

    /// self |= (src AND the mask's starts) shifted by the mask's length.
    func orShifted(_ src: SvSet, _ e: SvMask) {
        if src.isEmpty { return }
        // The first index whose word is at or after src.lo.
        var lo = 0
        var hi = e.idx.count
        while lo < hi {
            let mid = (lo + hi) / 2
            if e.idx[mid] < src.lo { lo = mid + 1 } else { hi = mid }
        }
        var n = lo
        while n < e.idx.count && e.idx[n] <= src.hi {
            let i = e.idx[n]
            let x = src.w[i] & e.bits[n]
            n += 1
            if x == 0 { continue }
            put(i, x << UInt64(e.k))
            if i + 1 < w.count { put(i + 1, x >> UInt64(64 - e.k)) }
        }
    }
}
