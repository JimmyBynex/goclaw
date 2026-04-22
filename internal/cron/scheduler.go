package cron

import (
	"context"
	"log"
	"time"
)

type Sender interface {
	ActiveSend(ctx context.Context, channelID, accountID, peerID, text string) error
}

type Scheduler struct {
	store  *Store
	sender Sender
}

func NewScheduler(store *Store, sender Sender) *Scheduler {
	return &Scheduler{store: store, sender: sender}
}

func (s *Scheduler) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		s.tick(ctx)
		for {
			select {
			case <-ticker.C:
				s.tick(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Scheduler) tick(ctx context.Context) {
	jobs, err := s.store.Due(time.Now())
	if err != nil {
		log.Printf("[cron] query due jobs error: %v", err)
		return
	}
	for _, j := range jobs {
		go s.execute(ctx, j)
	}
}

func (s *Scheduler) execute(ctx context.Context, j *Job) {
	log.Printf("[cron] executing job id=%d message=%q", j.ID, j.Message)
	err := s.sender.ActiveSend(ctx, j.ChannelID, j.AccountID, j.PeerID, j.Message)
	if err != nil {
		log.Printf("[cron] send failed job id=%d: %v", j.ID, err)
		return
	}
	if j.Repeat {
		next, err := ParseNextRun(j.Schedule, true, time.Now())
		if err != nil {
			log.Printf("[cron] parse schedule error job id=%d: %v", j.ID, err)
			return
		}
		s.store.UpdateNextRun(j.ID, next)
	} else {
		s.store.MarkDone(j.ID)
	}
}
