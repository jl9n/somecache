package master

import (
	"sync"
	"time"
)

type Host struct {
	lock    sync.Mutex
	host    string
	workers map[string]*Slave
}

func (h *Host) addSlave(port string, s *Slave) {
	h.lock.Lock()
	defer h.lock.Unlock()
	h.workers[port] = s
}

func (h *Host) delSlave(port string) {
	h.lock.Lock()
	defer h.lock.Unlock()
	delete(h.workers, port)
}

func (h *Host) getWorker() *Slave {
	h.lock.Lock()
	defer h.lock.Unlock()
	if len(h.workers) == 0 {
		return nil
	}
	index := time.Now().Nanosecond() % len(h.workers)
	i := 0
	for _, v := range h.workers {
		if i == index {
			return v
		}
		i++
	}
	return nil
}
