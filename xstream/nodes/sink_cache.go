package nodes

import (
	"encoding/gob"
	"fmt"
	"github.com/emqx/kuiper/common"
	"github.com/emqx/kuiper/xstream/api"
	"io"
	"path"
	"sort"
	"strconv"
)

type CacheTuple struct {
	index int
	data  interface{}
}

type LinkedQueue struct {
	Data map[int]interface{}
	Tail int
}

func (l *LinkedQueue) append(item interface{}) {
	l.Data[l.Tail] = item
	l.Tail++
}

func (l *LinkedQueue) delete(index int) {
	delete(l.Data, index)
}

func (l *LinkedQueue) reset() {
	l.Tail = 0
}

func (l *LinkedQueue) length() int {
	return len(l.Data)
}

func (l *LinkedQueue) clone() *LinkedQueue {
	result := &LinkedQueue{
		Data: make(map[int]interface{}),
		Tail: l.Tail,
	}
	for k, v := range l.Data {
		result.Data[k] = v
	}
	return result
}

func (l *LinkedQueue) String() string {
	return fmt.Sprintf("tail: %d, data: %v", l.Tail, l.Data)
}

type Cache struct {
	//Data and control channels
	in       <-chan interface{}
	Out      chan *CacheTuple
	Complete chan int
	errorCh  chan<- error
	//states
	pending *LinkedQueue
	//serialize
	key     string //the key for current cache
	store   common.KeyValue
	changed bool
	//configs
	limit        int
	saveInterval int
}

func NewCache(in <-chan interface{}, limit int, saveInterval int, errCh chan<- error, ctx api.StreamContext) *Cache {
	c := &Cache{
		in:       in,
		Out:      make(chan *CacheTuple, limit),
		Complete: make(chan int),
		errorCh:  errCh,

		limit:        limit,
		saveInterval: saveInterval,
	}
	go c.run(ctx)
	return c
}

func (c *Cache) run(ctx api.StreamContext) {
	logger := ctx.GetLogger()
	dbDir, err := common.GetAndCreateDataLoc("sink")
	logger.Debugf("cache saved to %s", dbDir)
	if err != nil {
		c.drainError(err)
	}
	c.store = common.GetSimpleKVStore(path.Join(dbDir, "cache"))
	c.key = ctx.GetRuleId() + ctx.GetOpId() + strconv.Itoa(ctx.GetInstanceId())
	//load cache
	if err := c.loadCache(); err != nil {
		go c.drainError(err)
		return
	}

	ticker := common.GetTicker(c.saveInterval)
	var tcount = 0
	for {
		select {
		case item := <-c.in:
			index := c.pending.Tail
			c.pending.append(item)
			//non blocking until limit exceeded
			c.Out <- &CacheTuple{
				index: index,
				data:  item,
			}
			c.changed = true
		case index := <-c.Complete:
			c.pending.delete(index)
			c.changed = true
		case <-ticker.C:
			tcount++
			l := c.pending.length()
			if l == 0 {
				c.pending.reset()
			}
			//If the data is still changing, only do a save when the cache has more than threshold to prevent too much file IO
			//If the data is not changing in the time slot and have not saved before, save it. This is to prevent the
			//data won't be saved as the cache never pass the threshold
			//logger.Infof("ticker %t, l=%d\n", c.changed, l)
			if (c.changed && l > common.Config.Sink.CacheThreshold) || (tcount == common.Config.Sink.CacheTriggerCount && c.changed) {
				logger.Infof("save cache for rule %s, %s", ctx.GetRuleId(), c.pending.String())
				clone := c.pending.clone()
				c.changed = false
				go func() {
					if err := c.saveCache(logger, clone); err != nil {
						logger.Debugf("%v", err)
						c.drainError(err)
					}
				}()
			}
			if tcount >= common.Config.Sink.CacheThreshold {
				tcount = 0
			}
		case <-ctx.Done():
			err := c.saveCache(logger, c.pending)
			if err != nil {
				logger.Warnf("Error found during saving cache: %s \n ", err)
			}
			logger.Infof("sink node %s instance cache %d done", ctx.GetOpId(), ctx.GetInstanceId())
			return
		}
	}
}

func (c *Cache) loadCache() error {
	c.pending = &LinkedQueue{
		Data: make(map[int]interface{}),
		Tail: 0,
	}
	gob.Register(c.pending)
	err := c.store.Open()
	if err != nil && err != io.EOF {
		return err
	}
	defer c.store.Close()
	if err == nil {
		if t, f := c.store.Get(c.key); f {
			if mt, ok := t.(*LinkedQueue); ok {
				c.pending = mt
				c.changed = true
				// To store the keys in slice in sorted order
				var keys []int
				for k := range mt.Data {
					keys = append(keys, k)
				}
				sort.Ints(keys)
				for _, k := range keys {
					t := &CacheTuple{
						index: k,
						data:  mt.Data[k],
					}
					c.Out <- t
				}
				return nil
			} else {
				return fmt.Errorf("load malform cache, found %t(%v)", t, t)
			}
		}
	}
	return nil
}

func (c *Cache) saveCache(logger api.Logger, p *LinkedQueue) error {
	err := c.store.Open()
	if err != nil {
		return err
	}
	defer c.store.Close()
	return c.store.Replace(c.key, p)
}

func (c *Cache) drainError(err error) {
	c.errorCh <- err
}

func (c *Cache) Length() int {
	return c.pending.length()
}
