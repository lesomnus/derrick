package copier

import (
	"bytes"
	"context"
	"io"
	"sync"
)

func bytesReader(b []byte) io.Reader {
	return bytes.NewReader(b)
}

// forEach runs fn over items with at most workers running at once.
//
// The first error cancels the rest and is what gets returned. A publish that
// has already gone wrong should stop rather than push more objects at a store
// that is refusing them.
func forEach[T any](ctx context.Context, items []T, workers int, fn func(context.Context, T) error) error {
	if len(items) == 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	queue := make(chan T)
	var (
		wg      sync.WaitGroup
		once    sync.Once
		failure error
	)

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range queue {
				if err := fn(ctx, item); err != nil {
					once.Do(func() {
						failure = err
						cancel()
					})

					return
				}
			}
		}()
	}

feed:
	for _, item := range items {
		select {
		case queue <- item:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()

	if failure != nil {
		return failure
	}

	return ctx.Err()
}
