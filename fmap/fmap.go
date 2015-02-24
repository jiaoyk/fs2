package fmap

/*
#include "fmap.h"
*/
import "C"

import (
	// "hash/crc32"
	"os"
	"reflect"
	"syscall"
	"unsafe"
)

import (
	"github.com/timtadh/fs2/errors"
	"github.com/timtadh/fs2/slice"
)

const BLOCKSIZE = 4096

type freeblk struct {
	next uint64
}

func loadFreeBlk(bytes []byte) *freeblk {
	free_s := slice.AsSlice(&bytes)
	return (*freeblk)(free_s.Array)
}

type ctrldata struct {
	// checksum  uint32
	blksize   uint32
	free_head uint64
	free_len  uint32
}

func (cd *ctrldata) Size() uintptr {
	return reflect.TypeOf(*cd).Size()
}

type ctrlblk struct {
	back []byte
	data *ctrldata
	user  []byte
}

func load_ctrlblk(bytes []byte) (cb *ctrlblk, err error) {
	back := slice.AsSlice(&bytes)
	data := (*ctrldata)(back.Array)
	ptr := uintptr(back.Array) + data.Size()
	// new_chksum := crc32.ChecksumIEEE(bytes[4:])
	// if new_chksum != data.checksum {
		// return nil, errors.Errorf("Bad control block checksum %x != %x", new_chksum, data.checksum)
	// }
	user_len := len(bytes) - int(data.Size())
	user_s := &slice.Slice{
		Array: unsafe.Pointer(ptr),
		Len: user_len,
		Cap: user_len,
	}
	user := *user_s.AsBytes()
	cb = &ctrlblk{
		back: bytes,
		data: data,
		user: user,
	}
	return cb, nil
}

func new_ctrlblk(bytes []byte, blksize uint32) (cb *ctrlblk) {
	back := slice.AsSlice(&bytes)
	data := (*ctrldata)(back.Array)
	ptr := uintptr(back.Array) + data.Size()
	data.blksize = blksize
	data.free_head = 0
	data.free_len = 0
	user_len := len(bytes) - int(data.Size())
	user_s := &slice.Slice{
		Array: unsafe.Pointer(ptr),
		Len: user_len,
		Cap: user_len,
	}
	user := *user_s.AsBytes()
	copy(user, make([]byte, len(user))) // zeros the user data
	// data.checksum = crc32.ChecksumIEEE(bytes[4:])
	cb = &ctrlblk{
		back: bytes,
		data: data,
		user: user,
	}
	return cb
}

func (cb *ctrlblk) Block() []byte {
	return cb.back
}

func (cb *ctrlblk) updateChkSum() {
	// cb.data.checksum = crc32.ChecksumIEEE(cb.back[4:])
}

// A BlockFile represents the memory mapped file. It has a blocksize all
// operations are done as block aligned operations.
type BlockFile struct {
	path   string
	opened bool
	size   uint64
	blksize int
	file   *os.File
	mmap unsafe.Pointer
	ptrs []int "outstanding pointers into mmap"
	outstanding int "total outstanding pointers"
}

// Zero the bytes of the passed in slice. It uses the length not the
// capacity of the slice. It uses the libc function memset under the
// hood to do this. Go has an implementation of this function in
// assembly in the runtime package but I could not find a nice way to
// expose it. So here is the libc version exposed (via several method
// calls and the cgo interface).
func MemClr(bytes []byte) {
	memClr(slice.AsSlice(&bytes).Array, uintptr(len(bytes)))
}

func memClr(ptr unsafe.Pointer, size uintptr) {
	C.memclr(ptr, C.size_t(size))
}

// Create a blockfile with the standard block size (4096 which is
// normally the OS page size).
func CreateBlockFile(path string) (*BlockFile, error) {
	return CreateBlockFileCustomBlockSize(path, BLOCKSIZE)
}

// Create a blockfile with a custom blocksize. Note, the size must be a
// multiple of 4096.
func CreateBlockFileCustomBlockSize(path string, size uint32) (*BlockFile, error) {
	if size % 4096 != 0 {
		panic(errors.Errorf("blocksize must be divisible by 4096"))
	}
	bf := &BlockFile{
		path: path,
		blksize: int(size),
		ptrs: make([]int, 1, size),
	}
	var err error
	bf.file, bf.mmap, err = create(path, size)
	if err != nil {
		return nil, err
	}
	bf.opened = true
	bf.size, err = bf.Size()
	if err != nil {
		return nil, err
	}
	err = bf.init_ctrl(size)
	if err != nil {
		return nil, err
	}
	return bf, nil
}

// Open a previously created BlockFile. This will fail if you didn't use
// the creation functions to create the file (or at least have undefined
// effects).
func OpenBlockFile(path string) (*BlockFile, error) {
	f, mmap, err := open(path)
	if err != nil {
		return nil, err
	}
	bf := &BlockFile{
		path: path,
		file: f,
		mmap: mmap,
		opened: true,
		blksize: BLOCKSIZE, // set the initial block size to a safe size
		ptrs: make([]int, 1, BLOCKSIZE), // also setup the initial pointers
	}
	bf.size, err = bf.Size()
	if err != nil {
		return nil, err
	}
	var blksize uint64
	err = bf.ctrl(func(ctrl *ctrlblk) error {
		blksize = uint64(ctrl.data.blksize)
		return nil
	})
	if err != nil {
		return nil, err
	}
	bf.blksize = int(blksize)
	blkcount := bf.size/blksize
	bf.ptrs = make([]int, blkcount, blkcount*2)
	return bf, nil
}

// The flag used when creating the file
var CREATEFLAG = os.O_RDWR | os.O_CREATE | syscall.O_NOATIME | os.O_TRUNC

func create(path string, blksize uint32) (*os.File, unsafe.Pointer, error) {
	f, err := do_open(path, CREATEFLAG)
	if err != nil {
		return nil, nil, err
	}
	err = f.Truncate(int64(blksize))
	if err != nil {
		return nil, nil, err
	}
	ptr, err := do_map(f)
	if err != nil {
		return nil, nil, err
	}
	return f, ptr, nil
}

// The flag used when opening the file
var OPENFLAG = os.O_RDWR | os.O_CREATE | syscall.O_NOATIME

func open(path string) (*os.File, unsafe.Pointer, error) {
	f, err := do_open(path, OPENFLAG)
	if err != nil {
		return nil, nil, err
	}
	ptr, err := do_map(f)
	if err != nil {
		return nil, nil, err
	}
	return f, ptr, nil
}

func do_open(path string, FLAG int) (*os.File, error) {
	f, err := os.OpenFile(path, FLAG, 0666)
	if err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func do_map(f *os.File) (unsafe.Pointer, error) {
	var mmap unsafe.Pointer = unsafe.Pointer(uintptr(0))
	errno := C.create_mmap(&mmap, C.int(f.Fd()))
	if errno != 0 {
		return nil, errors.Errorf("Could not create map fd = %d, %d", f.Fd(), errno)
	}
	return mmap, nil
}

// Close the file. Unmaps the region. There must be no outstanding
// blocks.
func (self *BlockFile) Close() error {
	if self.outstanding > 0 {
		return errors.Errorf("Tried to close file when there were outstanding pointers")
	}
	if errno := C.destroy_mmap(self.mmap, C.int(self.file.Fd())); errno != 0 {
		return errors.Errorf("destroy_mmap failed, %d", errno)
	}
	if err := self.file.Close(); err != nil {
		return err
	} else {
		self.file = nil
		self.opened = false
	}
	return nil
}

// Remove the underlying file. (must be already closed).
func (self *BlockFile) Remove() error {
	if self.opened {
		return errors.Errorf("Expected file to be closed")
	}
	return os.Remove(self.Path())
}

func (self *BlockFile) init_ctrl(blksize uint32) error {
	return self.Do(0, 1, func(bytes []byte) error {
		_ = new_ctrlblk(bytes, blksize)
		return self.Sync()
	})
}

func (self *BlockFile) ctrl(do func(*ctrlblk) error) (error) {
	return self.Do(0, 1, func(bytes []byte) error {
		cb, err := load_ctrlblk(bytes)
		if err != nil {
			return err
		}
		err = do(cb)
		cb.updateChkSum()
		return err
	})
}

// Get the "control data" this free form data which is stored in the
// control block file. You can put whatever you want in here.
func (self *BlockFile) ControlData() (data []byte, err error) {
	err = self.ctrl(func(ctrl *ctrlblk) error {
		data = make([]byte, len(ctrl.user))
		copy(data, ctrl.user)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Put user data into the control block of the file.
func (self *BlockFile) SetControlData(data []byte) (err error) {
	err = self.SetControlDataNoSync(data)
	if err != nil {
		return err
	}
	return self.Sync()
}

// Same as SetControlData but does not call Sync() at the end.
func (self *BlockFile) SetControlDataNoSync(data []byte) (err error) {
	return self.ctrl(func(ctrl *ctrlblk) error {
		if len(data) > len(ctrl.user) {
			return errors.Errorf("control data was too large")
		}
		copy(ctrl.user, data)
		return nil
	})
}

// The file system path to this file.
func (self *BlockFile) Path() string {
	return self.path
}

// The blocksize for this file.
func (self *BlockFile) BlockSize() int {
	return self.blksize
}

// The size of this file in bytes.
func (self *BlockFile) Size() (uint64, error) {
	if !self.opened {
		return 0, errors.Errorf("File is not open")
	}
	fi, err := self.file.Stat()
	if err != nil {
		return 0, err
	}
	return uint64(fi.Size()), nil
}

func (self *BlockFile) resize(size uint64) error {
	if self.outstanding > 0 {
		return errors.Errorf("cannot resize the file while there are outstanding pointers")
	}
	var new_mmap unsafe.Pointer
	errno := C.resize(self.mmap, &new_mmap, C.int(self.file.Fd()), C.size_t(size))
	if errno != 0 {
		return errors.Errorf("resize failed, %d", errno)
	}
	self.size = size
	self.mmap = new_mmap
	return nil
}

// Free the block at the given offset. The offset is in bytes from the
// start of the file.
func (self *BlockFile) Free(offset uint64) error {
	/*
	errno := C.is_normal(self.mmap, C.size_t(offset), C.size_t(self.blksize))
	if errno != 0 {
		return errors.Errorf("is_normal failed, %d", errno)
	}*/
	return self.ctrl(func(ctrl *ctrlblk) error {
		head := ctrl.data.free_head
		return self.Do(offset, 1, func(free_bytes []byte) error {
			free := loadFreeBlk(free_bytes)
			free.next = head
			ctrl.data.free_head = offset
			ctrl.data.free_len += 1
			return nil
		})
	})
}

func (self *BlockFile) pop_free() (offset uint64, err error) {
	err = self.ctrl(func(ctrl *ctrlblk) error {
		if ctrl.data.free_head == 0 || ctrl.data.free_len == 0 {
			return errors.Errorf("No blocks free")
		}
		offset = ctrl.data.free_head
		return self.Do(offset, 1, func(bytes []byte) error {
			free := loadFreeBlk(bytes)
			ctrl.data.free_head = free.next
			ctrl.data.free_len -= 1
			return nil
		})
	})
	if err != nil {
		return 0, err
	}
	return offset, nil
}

func (self *BlockFile) zero(offset uint64, n int) (uint64, error) {
	for i := 0; i < n; i++ {
		err := self.Do(offset + uint64(i*self.blksize), 1, func(block []byte) error {
			ptr := slice.AsSlice(&block).Array
			memClr(ptr, uintptr(len(block)))
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return offset, nil
}

func (self *BlockFile) alloc(n int) (offset uint64, err error) {
	start_size := self.size
	amt := uint64(self.blksize) * uint64(n)
	if err := self.resize(self.size + amt); err != nil {
		return 0, err
	}
	extra := make([]int, n)
	self.ptrs = append(self.ptrs, extra...)
	return start_size, nil
}

func (self *BlockFile) allocOne() (offset uint64, err error) {
	n := uint64(256)
	start_size := self.size
	amt := uint64(self.blksize) * n
	if err := self.resize(self.size + amt); err != nil {
		return 0, err
	}
	extra := make([]int, n)
	self.ptrs = append(self.ptrs, extra...)
	for i := uint64(1); i < n; i++ {
		o := i * uint64(self.blksize)
		err := self.Free(start_size + o)
		if err != nil {
			return 0, err
		}
	}
	return start_size, nil
}

// What is the address of the file in the address space of the program.
// Use this at your own risk!
func (self *BlockFile) Address() uintptr {
	return uintptr(self.mmap)
}

// Is the address given still the address of the memory map?
func (self *BlockFile) Valid(address uintptr) bool {
	return address == uintptr(self.mmap)
}

// Allocate 1 block and return its offset.
func (self *BlockFile) Allocate() (offset uint64, err error) {
	var resize bool = false
	err = self.ctrl(func(ctrl *ctrlblk) error {
		var err error
		if ctrl.data.free_len > 0 {
			offset, err = self.pop_free()
		} else {
			resize = true
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	if resize {
		offset, err = self.allocOne()
		if err != nil {
			return 0, err
		}
	}
	return self.zero(offset, 1)
}

// Allocate n blocks. Return the offset of the first block. These are
// guarranteed to be sequential. This always causes a file resize at the
// moment.
func (self *BlockFile) AllocateBlocks(n int) (offset uint64, err error) {
	offset, err = self.alloc(n)
	if err != nil {
		return 0, err
	}
	/*amt := uint64(self.blksize) * uint64(n)
	errno := C.is_sequential(self.mmap, C.size_t(offset), C.size_t(amt))
	if errno != 0 {
		return 0, errors.Errorf("is_sequential failed, %d", errno)
	}*/
	return self.zero(offset, n)
}

// Load the blocks at the give offset then call the callback, `do`,
// passing in the loaded bytes. This function releases those bytes after
// your callback is done. This is the recommended interface to the
// contents of the memory mapped region.
func (self *BlockFile) Do(offset, blocks uint64, do func([]byte) error) error {
	bytes, err := self.Get(offset, blocks)
	if err != nil {
		return err
	}
	defer self.Release(bytes)
	return do(bytes)
}

// Get the bytes at the offset and block count. You probably want to use
// Do instead. You must call Release() on the bytes when done.
func (self *BlockFile) Get(offset, blocks uint64) ([]byte, error) {
	length := blocks * uint64(self.blksize)
	if (offset + length) > uint64(self.size) {
		return nil, errors.Errorf("Get outside of the file, (%d) %d + %d > %d", offset + length, offset, length, self.size)
	}
	blk := (offset/uint64(self.blksize))
	for i := uint64(0); i < blocks; i++ {
		self.ptrs[blk + i] += 1
		self.outstanding += 1
	}
	slice := &slice.Slice{
		Array: unsafe.Pointer(uintptr(self.mmap) + uintptr(offset)),
		Len: int(length),
		Cap: int(length),
	}
	return *slice.AsBytes(), nil
}

// Release() bytes aquired with Get(). Should error if the bytes where
// not allocated from the mapping. But why take chances, you probably
// want to use the Do interface instead.
func (self *BlockFile) Release(bytes []byte) (error) {
	slice := slice.AsSlice(&bytes)
	length := uint64(slice.Len)
	blocks := length/uint64(self.blksize)
	offset := uint64(uintptr(slice.Array) - uintptr(self.mmap))
	blk := offset/uint64(self.blksize)
	for i := uint64(0); i < blocks; i++ {
		cblk := blk + i
		if cblk < 0 || cblk >= uint64(len(self.ptrs)) {
			return errors.Errorf("Tried to release a block that was not in this mapping")
		}
		if self.ptrs[cblk] <= 0 {
			return errors.Errorf("Tried to release block with no outstanding pointers (double free?)")
		}
		self.ptrs[cblk] -= 1
		self.outstanding -= 1
	}
	return nil
}

// Sync the mmap'ed changes to disk. This uses the async interface (via
// the MS_ASYNC flag) so the changes may not be written by the time this
// method returns. However, they will be written soon.
func (self *BlockFile) Sync() (error) {
	errno := C.sync_mmap(self.mmap, C.int(self.file.Fd()))
	if (errno != 0) {
		return errors.Errorf("sync_mmap failed, %d", errno)
	}
	return nil
}

