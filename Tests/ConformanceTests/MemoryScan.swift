#if os(macOS)
import Darwin

/// Searches the process's writable memory, freed memory included, for bytes that exist nowhere else: a needle made in
/// pages of the scan's own, which the search skips, so it never finds itself. A reader that copies the needle's bytes
/// and frees the copy unwiped leaves them in the heap, where the scan finds them.
final class MemoryScan {
    private let base: vm_address_t
    private let size: Int
    private static let chunkSize = 1 << 20
    /// Pages the scan skips: the needles, room for a file built from them, then the scan's read buffer.
    private let pages: UnsafeMutableRawBufferPointer
    private static let needleRoom = 1 << 12
    private var needlesUsed = 0
    private var used = needleRoom
    private let chunk: UnsafeMutableRawBufferPointer
    private var needles: [Range<Int>] = []

    init(room: Int = 1 << 16) {
        let page = Int(getpagesize())
        let wanted = (MemoryScan.needleRoom + room + page - 1) / page * page
        size = wanted + MemoryScan.chunkSize
        var address: vm_address_t = 0
        precondition(vm_allocate(mach_task_self_, &address, vm_size_t(size), VM_FLAGS_ANYWHERE) == KERN_SUCCESS)
        base = address
        let all = UnsafeMutableRawBufferPointer(start: UnsafeMutableRawPointer(bitPattern: address), count: size)
        pages = UnsafeMutableRawBufferPointer(rebasing: all[0..<wanted])
        chunk = UnsafeMutableRawBufferPointer(rebasing: all[wanted...])
    }

    deinit {
        _ = memset_s(UnsafeMutableRawPointer(bitPattern: base), size, 0, size)
        vm_deallocate(mach_task_self_, base, vm_size_t(size))
    }

    /// Makes `n` random lowercase letters in the skipped pages, apart from any document, and returns where they
    /// are: a needle.
    func needle(_ n: Int = 40) -> Range<Int> {
        precondition(needlesUsed + n <= MemoryScan.needleRoom, "out of needle room")
        let r = needlesUsed..<(needlesUsed + n)
        let out = UnsafeMutableRawBufferPointer(rebasing: pages[r])
        arc4random_buf(out.baseAddress, out.count)
        for k in out.indices { out[k] = 0x61 + out[k] % 26 }
        needlesUsed += n
        needles.append(r)
        return r
    }

    /// Counts the first `n` bytes of the needle at `r` as a needle of their own, and returns where they are: a buffer
    /// outgrown part way through a copy holds them without the rest. Every whole copy holds them too.
    @discardableResult
    func prefix(_ r: Range<Int>, _ n: Int) -> Range<Int> {
        precondition(n <= r.count, "a prefix longer than its needle")
        let p = r.lowerBound..<(r.lowerBound + n)
        needles.append(p)
        return p
    }

    /// Rewrites the needle at `r` as digits without a leading zero, so it is a number's text.
    func digits(_ r: Range<Int>) {
        let out = UnsafeMutableRawBufferPointer(rebasing: pages[r])
        for k in out.indices { out[k] = 0x31 + out[k] % 9 }
    }

    /// Appends the bytes `body` writes into `count` bytes of the skipped pages.
    @discardableResult
    func append(count: Int, _ body: (UnsafeMutableRawBufferPointer) -> Void) -> Range<Int> {
        precondition(used + count <= pages.count, "out of room")
        let r = used..<(used + count)
        body(UnsafeMutableRawBufferPointer(rebasing: pages[r]))
        used += count
        return r
    }

    func append(_ s: String) { var s = s; _ = s.withUTF8 { u in append(count: u.count) { $0.copyBytes(from: u) } } }

    /// Appends the bytes at `r`, already in the pages.
    func append(copyOf r: Range<Int>) {
        append(count: r.count) { $0.copyMemory(from: UnsafeRawBufferPointer(rebasing: pages[r])) }
    }

    func bytes(_ r: Range<Int>) -> UnsafeRawBufferPointer { UnsafeRawBufferPointer(rebasing: pages[r]) }

    /// Clears what was appended after `mark`, so the next document is built from there.
    func reset(to mark: Int) {
        let r = mark..<used
        _ = memset_s(pages.baseAddress! + mark, r.count, 0, r.count)
        used = mark
    }

    var mark: Int { used }

    /// Whether a sanitizer's allocator replaced the system's. It never calls `malloc_logger`, so ``frees(during:)``
    /// sees nothing and a test of "no copy left" would pass without checking: under it the scan cannot be used.
    static var allocatorReplaced: Bool {
        let rtldDefault = UnsafeMutableRawPointer(bitPattern: -2)
        return ["__tsan_init", "__asan_init"].contains { (name: String) in dlsym(rtldDefault, name) != nil }
    }

    /// Runs `body` and returns how many blocks were freed, each needle counted apart, while holding the needle's
    /// bytes: the allocator's logger sees every free before it happens, so a copy the system zeroes on free (small
    /// blocks, on recent macOS) is still found. The logger allocates nothing.
    func frees(during body: () throws -> Void) rethrows -> [Int] {
        let counts = UnsafeMutablePointer<Int>.allocate(capacity: max(1, needles.count))
        counts.initialize(repeating: 0, count: max(1, needles.count))
        let list = UnsafeMutablePointer<(UnsafeRawPointer, Int)>.allocate(capacity: max(1, needles.count))
        for (k, r) in needles.enumerated() { list[k] = (UnsafeRawPointer(pages.baseAddress! + r.lowerBound), r.count) }
        defer {
            counts.deallocate()
            list.deallocate()
        }
        let slot = FreeWatch.slot()
        os_unfair_lock_lock(FreeWatch.lock)
        FreeWatch.state = (list, needles.count, counts)
        os_unfair_lock_unlock(FreeWatch.lock)
        slot.pointee = FreeWatch.logger
        defer {
            slot.pointee = nil
            os_unfair_lock_lock(FreeWatch.lock)
            FreeWatch.state = nil
            os_unfair_lock_unlock(FreeWatch.lock)
        }
        try body()
        return (0..<needles.count).map { counts[$0] }
    }

    /// How many times each needle appears in writable memory outside the skipped pages.
    func occurrences() -> [Int] {
        var counts = [Int](repeating: 0, count: needles.count)
        let skipped = UInt(base)..<UInt(base + vm_address_t(size))
        var address: mach_vm_address_t = 0
        var depth: natural_t = 0
        while true {
            var length: mach_vm_size_t = 0
            var info = vm_region_submap_info_data_64_t()
            var count = mach_msg_type_number_t(
                MemoryLayout<vm_region_submap_info_data_64_t>.size / MemoryLayout<natural_t>.size)
            let kr = withUnsafeMutablePointer(to: &info) {
                $0.withMemoryRebound(to: Int32.self, capacity: Int(count)) {
                    mach_vm_region_recurse(mach_task_self_, &address, &length, &depth, $0, &count)
                }
            }
            guard kr == KERN_SUCCESS else { break }
            if info.is_submap != 0 {
                depth += 1
                continue
            }
            let readable = info.protection & VM_PROT_READ != 0 && info.protection & VM_PROT_WRITE != 0
            if readable && (info.pages_resident > 0 || info.pages_swapped_out > 0) {
                let region = UInt(address)..<UInt(address + length)
                // The skipped pages may share a region with their neighbours.
                for part in [region.lowerBound..<min(region.upperBound, max(region.lowerBound, skipped.lowerBound)),
                    max(region.lowerBound, min(region.upperBound, skipped.upperBound))..<region.upperBound]
                where !part.isEmpty {
                    scan(part, &counts)
                }
            }
            address += length
        }
        return counts
    }

    private func scan(_ range: Range<UInt>, _ counts: inout [Int]) {
        let longest = needles.map(\.count).max() ?? 0
        var at = range.lowerBound
        while at < range.upperBound {
            let n = min(UInt(MemoryScan.chunkSize), range.upperBound - at)
            var got: mach_vm_size_t = 0
            let kr = mach_vm_read_overwrite(
                mach_task_self_, mach_vm_address_t(at), mach_vm_size_t(n),
                mach_vm_address_t(UInt(bitPattern: chunk.baseAddress)), &got)
            if kr == KERN_SUCCESS, got > 0 {
                for (k, r) in needles.enumerated() {
                    var from = chunk.baseAddress!
                    var left = Int(got)
                    let needle = pages.baseAddress! + r.lowerBound
                    while left >= r.count, let hit = memmem(from, left, needle, r.count) {
                        // A hit starting in the overlap is counted by the next chunk, which starts there.
                        let offset = hit - chunk.baseAddress!
                        if at + n >= range.upperBound || offset < Int(n) - (longest - 1) { counts[k] += 1 }
                        from = hit + 1
                        left = Int(got) - (offset + 1)
                    }
                }
            }
            _ = memset_s(chunk.baseAddress, chunk.count, 0, chunk.count)
            guard at + n < range.upperBound else { break }
            at += n - UInt(longest - 1)
        }
    }
}
/// The allocator's logger (libmalloc's `malloc_logger`), set only while ``MemoryScan/frees(during:)`` runs.
private enum FreeWatch {
    typealias Logger = @convention(c) (UInt32, UInt, UInt, UInt, UInt, UInt32) -> Void

    nonisolated(unsafe) static var state: (needles: UnsafeMutablePointer<(UnsafeRawPointer, Int)>, count: Int,
        counts: UnsafeMutablePointer<Int>)?
    nonisolated(unsafe) static let lock: os_unfair_lock_t = {
        let l = os_unfair_lock_t.allocate(capacity: 1)
        l.initialize(to: os_unfair_lock())
        return l
    }()

    static func slot() -> UnsafeMutablePointer<Logger?> {
        let rtldDefault = UnsafeMutableRawPointer(bitPattern: -2)
        guard let p = dlsym(rtldDefault, "malloc_logger") else { fatalError("no malloc_logger") }
        return p.assumingMemoryBound(to: Logger?.self)
    }

    static let logger: Logger = { type, arg1, arg2, _, _, _ in
        let deallocate: UInt32 = 4, hasZone: UInt32 = 8
        guard type & deallocate != 0 else { return }
        guard let block = UnsafeRawPointer(bitPattern: type & hasZone != 0 ? arg2 : arg1) else { return }
        let size = malloc_size(block)
        guard size > 0 else { return }
        // Only frees made by the watched code count: one the logger's own work makes, if any, finds the lock held.
        guard os_unfair_lock_trylock(lock) else { return }
        defer { os_unfair_lock_unlock(lock) }
        guard let s = state else { return }
        var k = 0
        while k < s.count {
            if memmem(block, size, s.needles[k].0, s.needles[k].1) != nil { s.counts[k] += 1 }
            k += 1
        }
    }
}
#endif
