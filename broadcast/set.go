package broadcast

import (
	"sync"
	"time"
)

type Set struct {
	mutex   sync.RWMutex // protects `streams`
	streams map[string]*Single
	// There is a timeout after releasing a stream during which it is possible
	// to reacquire the same object and continue broadcasting. Once the timeout
	// elapses, the stream is closed for good.
	Timeout time.Duration
	// When a stream is actually closed, this function is called as a notification.
	OnStreamClose func(id string)
}

type Single struct {
	Broadcast
	closing            bool
	closingStateChange chan bool

	Created time.Time
	// These values are for the whole stream, so they include audio and muxing overhead.
	// The latter is negligible, however, and the former is normally about 64k,
	// so also negligible. Or at least predictable.
	Rate struct {
		Mean float64
		Var  float64
		unit float64
	}
}

// Acquire a stream for writing. Only one "writable" reference can be held;
// until it is closed, this function will return an error.
func (ctx *Set) Acquire(id string) (*Single, bool) {
	ctx.mutex.Lock()
	defer ctx.mutex.Unlock()
	if ctx.streams == nil {
		ctx.streams = make(map[string]*Single)
	}
	stream, ok := ctx.streams[id]
	if !ok {
		v := &Single{
			Broadcast:          NewBroadcast(),
			Created:            time.Now().UTC(),
			closingStateChange: make(chan bool),
		}
		ctx.streams[id] = v
		go v.monitor(ctx, id)
		return v, true
	}
	if !stream.closing {
		return nil, false
	}
	stream.closingStateChange <- false
	return stream, true
}

// Acquire a stream for reading. There is no limit on the number of concurrent readers.
func (ctx *Set) Get(id string) (*Single, bool) {
	if ctx.streams == nil {
		return nil, false
	}
	ctx.mutex.RLock()
	stream, ok := ctx.streams[id]
	ctx.mutex.RUnlock()
	return stream, ok
}

func (stream *Single) monitor(ctx *Set, id string) {
	ticker := time.NewTicker(time.Second)
	ticksWhileOffline := 0 * time.Second

	for {
		select {
		case stream.closing = <-stream.closingStateChange:
			ticksWhileOffline = 0
		case <-ticker.C:
			if stream.closing {
				if ticksWhileOffline += time.Second; ticksWhileOffline > ctx.Timeout {
					ctx.mutex.Lock()
					defer ctx.mutex.Unlock()
					delete(ctx.streams, id)
					ticker.Stop()
					stream.Broadcast.Close()
					if ctx.OnStreamClose != nil {
						ctx.OnStreamClose(id)
					}
					return
				}
			}
			// exponentially weighted moving moments at a = 0.5
			//     avg[n] = a * x + (1 - a) * avg[n - 1]
			//     var[n] = a * (x - avg[n]) ** 2 / (1 - a) + (1 - a) * var[n - 1]
			stream.Rate.Mean += stream.Rate.unit / 2
			stream.Rate.Var += stream.Rate.unit*stream.Rate.unit - stream.Rate.Var/2
			stream.Rate.unit = -stream.Rate.Mean
		}
	}
}

func (stream *Single) Write(data []byte) (int, error) {
	stream.Rate.unit += float64(len(data))
	return stream.Broadcast.Write(data)
}

func (stream *Single) Close() error {
	stream.closingStateChange <- true
	return nil
}