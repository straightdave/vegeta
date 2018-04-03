package trunks

import (
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"time"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type GTargeter struct {
	Target       string
	IsEtcd       bool
	MethodName   string
	Requests     []*interface{}
	ResponseType reflect.Type
}

// create an argument for gRPC invoke
func (t *GTargeter) Request() interface{} {
	return nil
}

// create a response for gRPC invoke
func (t *GTargeter) Response() interface{} {
	return nil
}

type Burner struct {
	Conn    *grpc.ClientConn
	Workers uint64
	Ctx     context.Context
	stopch  chan struct{}
}

// since Target could be Etcd, the connection may be in a different way
// so Burnner (connection owner and initializer) comes from target
func (t *GTargeter) GenBurner() (burner *Burner, err error) {
	if t.IsEtcd {
		return nil, fmt.Errorf("Etcd is not supported yet")
	}

	// directy dialing
	c, err := grpc.Dial(t.Target, grpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	// healthy check
	grpcCheck := grpc_health_v1.NewHealthClient(c)
	checkReq := &grpc_health_v1.HealthCheckRequest{
		Service: "",
	}

	_, checkErr := grpcCheck.Check(context.Background(), checkReq)
	if checkErr != nil {
		c.Close()
		return nil, fmt.Errorf("Not Healthy")
	}

	return &Burner{
		Conn:    c,
		Workers: uint64(runtime.NumCPU()),
		Ctx:     context.Background(),
	}, nil
}

func (b *Burner) Burn(tgt GTargeter, rate uint64, du time.Duration) <-chan *Result {

	var workers sync.WaitGroup
	results := make(chan *Result)
	ticks := make(chan time.Time)
	for i := uint64(0); i < b.Workers; i++ {
		workers.Add(1)
		go b.burn(tgt, &workers, ticks, results)
	}

	go func() {
		defer close(results)
		defer workers.Wait()
		defer close(ticks)
		interval := 1e9 / rate
		hits := rate * uint64(du.Seconds())
		began, done := time.Now(), uint64(0)
		for {
			now, next := time.Now(), began.Add(time.Duration(done*interval))
			time.Sleep(next.Sub(now))
			select {
			case ticks <- max(next, now):
				if done++; done == hits {
					return
				}
			case <-b.stopch:
				return
			default: // all workers are blocked. start one more and try again
				workers.Add(1)
				go b.burn(tgt, &workers, ticks, results)
			}
		}
	}()

	return results
}

func (b *Burner) Stop() {
	select {
	case <-b.stopch:
		return
	default:
		close(b.stopch)
	}
}

func (b *Burner) burn(tgt GTargeter, workers *sync.WaitGroup, ticks <-chan time.Time, results chan<- *Result) {
	defer workers.Done()
	for tm := range ticks {
		results <- b.hit(tgt, tm)
	}
}

func (b *Burner) hit(tgt GTargeter, tm time.Time) *Result {
	var res = Result{Timestamp: tm}
	var err error

	defer func() {
		res.Latency = time.Since(tm)
		if err != nil {
			res.Error = err.Error()
		}
	}()

	req := tgt.Request()
	resp := tgt.Response()

	if err := b.Conn.Invoke(b.Ctx, tgt.MethodName, &req, &resp, nil); err != nil {
		res.Error = err.Error()
	}

	return &res
}
