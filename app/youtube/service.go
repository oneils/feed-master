// Package youtube provides loading audio from video files for given youtube channels
package youtube

import (
	"context"
	"crypto/sha1"
	"encoding/xml"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/google/uuid"
	"github.com/pkg/errors"

	rssfeed "github.com/umputun/feed-master/app/feed"
	ytfeed "github.com/umputun/feed-master/app/youtube/feed"
)

//go:generate moq -out mocks/downloader.go -pkg mocks -skip-ensure -fmt goimports . DownloaderService
//go:generate moq -out mocks/channel.go -pkg mocks -skip-ensure -fmt goimports . ChannelService
//go:generate moq -out mocks/store.go -pkg mocks -skip-ensure -fmt goimports . StoreService

// Service loads audio from youtube channels
type Service struct {
	Feeds          []FeedInfo
	Downloader     DownloaderService
	ChannelService ChannelService
	Store          StoreService
	CheckDuration  time.Duration
	RSSFileStore   RSSFileStore
	KeepPerChannel int
	RootURL        string
}

// FeedInfo contains channel or feed ID, readable name and other per-feed info
type FeedInfo struct {
	Name     string      `yaml:"name"`
	ID       string      `yaml:"id"`
	Type     ytfeed.Type `yaml:"type"`
	Keep     int         `yaml:"keep"`
	Language string      `yaml:"lang"`
}

// DownloaderService is an interface for downloading audio from youtube
type DownloaderService interface {
	Get(ctx context.Context, id string, fname string) (file string, err error)
}

// ChannelService is an interface for getting channel entries, i.e. the list of videos
type ChannelService interface {
	Get(ctx context.Context, chanID string, feedType ytfeed.Type) ([]ytfeed.Entry, error)
}

// StoreService is an interface for storing and loading metadata about downloaded audio
type StoreService interface {
	Save(entry ytfeed.Entry) (bool, error)
	Load(channelID string, max int) ([]ytfeed.Entry, error)
	Exist(entry ytfeed.Entry) (bool, error)
	RemoveOld(channelID string, keep int) ([]string, error)
	SetProcessed(entry ytfeed.Entry) error
	CheckProcessed(entry ytfeed.Entry) (found bool, ts time.Time, err error)
	CountProcessed() (count int)
	Last() (ytfeed.Entry, error)
}

// Do is a blocking function that downloads audio from youtube channels and updates metadata
func (s *Service) Do(ctx context.Context) error {
	log.Printf("[INFO] starting youtube service")

	for _, f := range s.Feeds {
		log.Printf("[INFO] youtube feed %+v", f)
	}

	tick := time.NewTicker(s.CheckDuration)
	defer tick.Stop()

	if err := s.procChannels(ctx); err != nil {
		return errors.Wrap(err, "failed to process channels")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := s.procChannels(ctx); err != nil {
				return errors.Wrap(err, "failed to process channels")
			}
		}
	}
}

// RSSFeed generates RSS feed for given channel
func (s *Service) RSSFeed(fi FeedInfo) (string, error) {
	entries, err := s.Store.Load(fi.ID, s.keep(fi))
	if err != nil {
		return "", errors.Wrap(err, "failed to get channel entries")
	}

	if len(entries) == 0 {
		return "", nil
	}

	items := []rssfeed.Item{}
	for _, entry := range entries {

		fileURL := s.RootURL + "/" + path.Base(entry.File)

		var fileSize int
		if fileInfo, fiErr := os.Stat(entry.File); fiErr != nil {
			log.Printf("[WARN] failed to get file size for %s (%s %s): %v",
				entry.File, entry.VideoID, entry.Title, fiErr)
		} else {
			fileSize = int(fileInfo.Size())
		}

		items = append(items, rssfeed.Item{
			Title:       entry.Title,
			Description: entry.Media.Description,
			Link:        entry.Link.Href,
			PubDate:     entry.Published.Format(time.RFC822Z),
			GUID:        entry.ChannelID + "::" + entry.VideoID,
			Author:      entry.Author.Name,
			Enclosure: rssfeed.Enclosure{
				URL:    fileURL,
				Type:   "audio/mpeg",
				Length: fileSize,
			},
			DT: time.Now(),
		})
	}

	rss := rssfeed.Rss2{
		Version:       "2.0",
		ItemList:      items,
		Title:         fi.Name,
		Description:   "generated by feed-master",
		Link:          entries[0].Author.URI,
		PubDate:       items[0].PubDate,
		LastBuildDate: time.Now().Format(time.RFC822Z),
		Language:      fi.Language,
	}

	if fi.Type == ytfeed.FTPlaylist {
		rss.Link = "https://www.youtube.com/playlist?list=" + fi.ID
	}

	b, err := xml.MarshalIndent(&rss, "", "  ")
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal rss")
	}

	return string(b), nil
}

func (s *Service) procChannels(ctx context.Context) error {

	var allStats stats

	for _, feedInfo := range s.Feeds {
		entries, err := s.ChannelService.Get(ctx, feedInfo.ID, feedInfo.Type)
		if err != nil {
			log.Printf("[WARN] failed to get channel entries for %s: %s", feedInfo.ID, err)
			continue
		}
		log.Printf("[INFO] got %d entries for %s, limit to %d", len(entries), feedInfo.Name, s.keep(feedInfo))
		changed, processed := false, 0
		for i, entry := range entries {
			allStats.entries++
			if processed >= s.keep(feedInfo) {
				break
			}

			// check if entry already exists in store
			// this method won't work after migration to locally altered published ts but have to stay for now
			// to avoid false-positives on old entries what never got set with SetProcessed
			exists, exErr := s.Store.Exist(entry)
			if err != nil {
				return errors.Wrapf(exErr, "failed to check if entry %s exists", entry.VideoID)
			}
			if exists {
				allStats.skipped++
				processed++
				continue
			}

			// check if we already processed this entry.
			// this is needed to avoid infinite get/remove loop when the original feed is updated in place.
			// after migration to locally altered published ts, it is also the primary way to detect already processed entries
			found, _, procErr := s.Store.CheckProcessed(entry)
			if procErr != nil {
				log.Printf("[WARN] can't get processed status for %s, %+v", entry.VideoID, feedInfo)
			}
			if procErr == nil && found {
				allStats.skipped++
				processed++
				continue
			}

			log.Printf("[INFO] new entry [%d] %s, %s, %s", i+1, entry.VideoID, entry.Title, feedInfo.Name)
			file, downErr := s.Downloader.Get(ctx, entry.VideoID, s.makeFileName(entry))
			if downErr != nil {
				allStats.ignored++
				log.Printf("[WARN] failed to download %s: %s", entry.VideoID, downErr)
				continue
			}
			processed++
			log.Printf("[INFO] downloaded %s (%s) to %s, channel: %+v", entry.VideoID, entry.Title, file, feedInfo)

			entry.File = file

			// only reset time if published not too while ago
			// this is to avoid initial set of entries added with a new channel
			if time.Since(entry.Published) < time.Hour*24 {
				entry.Published = time.Now() // set published to prevent possible out-of-order entries
			}

			if !strings.Contains(entry.Title, feedInfo.Name) { // if title doesn't contains channel name add it
				entry.Title = feedInfo.Name + ": " + entry.Title
			}
			ok, saveErr := s.Store.Save(entry)
			if saveErr != nil {
				return errors.Wrapf(saveErr, "failed to save entry %+v", entry)
			}
			if !ok {
				log.Printf("[WARN] attempt to save dup entry %+v", entry)
			}
			changed = true
			if procErr = s.Store.SetProcessed(entry); procErr != nil {
				log.Printf("[WARN] failed to set processed status for %s: %v", entry.VideoID, procErr)
			}
			allStats.added++
			log.Printf("[INFO] saved %s (%s) to %s, channel: %+v", entry.VideoID, entry.Title, file, feedInfo)
		}
		allStats.processed += processed

		if changed { // save rss feed to fs if there are new entries
			removed := s.removeOld(feedInfo)
			allStats.removed += removed

			rss, rssErr := s.RSSFeed(feedInfo)
			if rssErr != nil {
				log.Printf("[WARN] failed to generate rss for %s: %s", feedInfo.Name, rssErr)
			} else {
				if err := s.RSSFileStore.Save(feedInfo.ID, rss); err != nil {
					log.Printf("[WARN] failed to save rss for %s: %s", feedInfo.Name, err)
				}
			}
		}
	}

	log.Printf("[INFO] all channels processed - channels: %d, %s, lifetime: %d", len(s.Feeds), allStats.String(), s.Store.CountProcessed())

	if last, err := s.Store.Last(); err == nil {
		log.Printf("[INFO] last entry: %s", last.String())
	}

	return nil
}

// removeOld deletes old entries from store and corresponding files
func (s *Service) removeOld(fi FeedInfo) int {
	removed := 0
	keep := s.keep(fi)
	files, err := s.Store.RemoveOld(fi.ID, keep+1)
	if err != nil { // even with error we get a list of files to remove
		log.Printf("[WARN] failed to remove some old meta data for %s, %v", fi.ID, err)
	}

	for _, f := range files {
		if e := os.Remove(f); e != nil {
			log.Printf("[WARN] failed to remove file %s: %v", f, e)
			continue
		}
		removed++
		log.Printf("[INFO] removed %s for %s (%s)", f, fi.ID, fi.Name)
	}
	return removed
}

func (s *Service) keep(fi FeedInfo) int {
	keep := s.KeepPerChannel
	if fi.Keep > 0 {
		keep = fi.Keep
	}
	return keep
}

func (s *Service) makeFileName(entry ytfeed.Entry) string {
	h := sha1.New()
	if _, err := h.Write([]byte(entry.ChannelID + "::" + entry.VideoID)); err != nil {
		return uuid.New().String()
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

type stats struct {
	entries   int
	processed int
	added     int
	removed   int
	ignored   int
	skipped   int
}

func (st stats) String() string {
	return fmt.Sprintf("entries: %d, processed: %d, updated: %d, removed: %d, ignored: %d, skipped: %d",
		st.entries, st.processed, st.added, st.removed, st.ignored, st.skipped)
}
