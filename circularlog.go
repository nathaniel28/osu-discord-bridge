package main

import (
	"sync/atomic"

	"golang.org/x/sys/unix"
)

type CircularLog struct {
	// memory mapped log buffer
	buf []byte

	// next place to write
	// may be greater than len(buf), so always take pos % len(buf)
	//pos atomic.Uint32
	pos atomic.Uint64
}

func (cl *CircularLog) Open(path string, size int) (err error) {
	var fd int
	fd, err = unix.Open(path, unix.O_RDWR|unix.O_CREAT, 0644)
	if err != nil {
		return
	}
	err = unix.Fallocate(fd, 0, 0, int64(size))
	if err != nil {
		return
	}
	cl.buf, err = unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	unix.Close(fd)
	return
}

func (cl *CircularLog) Write(p []byte) (n int, err error) {
	n = len(p)
	from := (cl.pos.Add(uint64(n)) - uint64(n)) % uint64(len(cl.buf))
	w := copy(cl.buf[from:], p)
	if w < n {
		copy(cl.buf, p[w:])
	}
	return
}

func (cl *CircularLog) Close() (err error) {
	if cl.buf == nil {
		return
	}
	err = unix.Munmap(cl.buf)
	if err == nil {
		cl.buf = nil
	}
	return
}
