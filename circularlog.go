package main

import (
	"log"
	"sync"

	"golang.org/x/sys/unix"
)

type CircularLog struct {
	buf  []byte
	Log  *log.Logger
	lock sync.Mutex
	pos  int
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
	if err == nil {
		cl.Log = log.New(cl, "", log.Ldate|log.Ltime)
	}
	return
}

func (cl *CircularLog) Write(p []byte) (n int, err error) {
	// TODO: use a fancier locking mechanism that only blocks if the part of
	// the buffer you want to write to is in use?
	cl.lock.Lock()
	n = len(p)
	if n > len(cl.buf) {
		n = len(cl.buf)
		p = p[:n]
	}
	w := copy(cl.buf[cl.pos:], p)
	if w == len(p) {
		cl.pos += w
	} else {
		cl.pos = copy(cl.buf, p[w:])
	}
	cl.lock.Unlock()
	return
}

func (cl *CircularLog) Close() (err error) {
	if cl.buf == nil {
		return
	}
	err = unix.Munmap(cl.buf)
	if err == nil {
		cl.Log = nil
		cl.buf = nil
	}
	return
}
