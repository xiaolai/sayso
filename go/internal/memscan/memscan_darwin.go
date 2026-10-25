//go:build darwin && cgo

package memscan

/*
#include <mach/mach.h>
#include <mach/mach_vm.h>

// Calls back into Go with each writable region holding pages. Returns the status that ended the walk:
// KERN_INVALID_ADDRESS once no region lies past the last one, anything else a failure.
extern void memscanRegion(mach_vm_address_t lo, mach_vm_address_t hi);

static kern_return_t memscanRegions(void) {
	mach_vm_address_t address = 0;
	natural_t depth = 0;
	for (;;) {
		mach_vm_size_t size = 0;
		vm_region_submap_info_data_64_t info;
		mach_msg_type_number_t count = VM_REGION_SUBMAP_INFO_COUNT_64;
		kern_return_t kr = mach_vm_region_recurse(mach_task_self(), &address, &size, &depth, (vm_region_recurse_info_t)&info, &count);
		if (kr != KERN_SUCCESS) {
			return kr;
		}
		if (info.is_submap) {
			depth++;
			continue;
		}
		if ((info.protection & VM_PROT_READ) && (info.protection & VM_PROT_WRITE) && (info.pages_resident > 0 || info.pages_swapped_out > 0)) {
			memscanRegion(address, address + size);
		}
		address += size;
	}
}

static mach_vm_size_t memscanRead(mach_vm_address_t a, void *out, mach_vm_size_t n) {
	mach_vm_size_t got = 0;
	if (mach_vm_read_overwrite(mach_task_self(), a, n, (mach_vm_address_t)out, &got) != KERN_SUCCESS) {
		return 0;
	}
	return got;
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

var (
	mu    sync.Mutex
	found [][2]uintptr
)

//export memscanRegion
func memscanRegion(lo, hi C.mach_vm_address_t) {
	found = append(found, [2]uintptr{uintptr(lo), uintptr(hi)})
}

// regions calls f with each writable region holding pages, touching regions joined. They are listed first and
// visited after, so the visit's own allocations do not change the list as it is walked. A walk that ends in anything
// but the end of the address space fails the scan rather than leave the regions past it unscanned.
func regions(f func(lo, hi uintptr)) error {
	mu.Lock()
	defer mu.Unlock()
	found = found[:0]
	if kr := C.memscanRegions(); kr != C.KERN_INVALID_ADDRESS {
		return fmt.Errorf("memscan: listing the memory regions failed: kern_return_t %d", int(kr))
	}
	spans, _ := union(found)
	for _, r := range spans {
		f(r[0], r[1])
	}
	return nil
}

// read copies memory at a into b and returns how much it could.
func read(a uintptr, b []byte) int {
	return int(C.memscanRead(C.mach_vm_address_t(a), unsafe.Pointer(&b[0]), C.mach_vm_size_t(len(b))))
}
