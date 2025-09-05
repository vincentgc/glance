package glance

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	// "net/url"
	"sort"
	"strings"
	"time"
	"os"
	"crypto/md5"
	"io"
	"path/filepath"
	"sync"
)

const videosWidgetPlaylistPrefix = "playlist:"

var (
	videosWidgetTemplate             = mustParseTemplate("videos.html", "widget-base.html", "video-card-contents.html")
	videosWidgetGridTemplate         = mustParseTemplate("videos-grid.html", "widget-base.html", "video-card-contents.html")
	videosWidgetVerticalListTemplate = mustParseTemplate("videos-vertical-list.html", "widget-base.html")
)

type videosWidget struct {
	widgetBase        `yaml:",inline"`
	Videos            videoList `yaml:"-"`
	VideoUrlTemplate  string    `yaml:"video-url-template"`
	Style             string    `yaml:"style"`
	CollapseAfter     int       `yaml:"collapse-after"`
	CollapseAfterRows int       `yaml:"collapse-after-rows"`
	Channels          []string  `yaml:"channels"`
	Playlists         []string  `yaml:"playlists"`
	Limit             int       `yaml:"limit"`
	IncludeShorts     bool      `yaml:"include-shorts"`
}

type bilibiliSpaceResponseJson struct {
	Data struct {
		Item []struct {
			Title  string `json:"title"`
			Cover  string `json:"cover"`
			Ctime  int64  `json:"ctime"`
			Author string `json:"author"`
			Bvid   string `json:"bvid"`
		} `json:"item"`
	} `json:"data"`
}

// 图片缓存管理器
type ImageCache struct {
    cacheDir      string
    cacheDuration time.Duration
    downloading   map[string]chan struct{} // 防止重复下载
    mutex         sync.RWMutex
}

// 创建图片缓存管理器
func NewImageCache(cacheDir string, duration time.Duration) *ImageCache {
    // 确保缓存目录存在
    if err := os.MkdirAll(cacheDir, 0755); err != nil {
        slog.Error("Failed to create cache directory", "dir", cacheDir, "error", err)
    }

    return &ImageCache{
        cacheDir:      cacheDir,
        cacheDuration: duration,
        downloading:   make(map[string]chan struct{}),
    }
}

// 生成缓存文件名
func (ic *ImageCache) getCacheFileName(url string) string {
    hash := md5.Sum([]byte(url))
    
    // 根据URL确定文件扩展名
    ext := ".jpg" // 默认
    if strings.Contains(url, ".png") {
        ext = ".png"
    } else if strings.Contains(url, ".webp") {
        ext = ".webp"
    } else if strings.Contains(url, ".gif") {
        ext = ".gif"
    }
    
    return fmt.Sprintf("%x%s", hash, ext)
}

// 获取缓存文件完整路径
func (ic *ImageCache) getCacheFilePath(url string) string {
    return filepath.Join(ic.cacheDir, ic.getCacheFileName(url))
}

// 检查缓存是否有效
func (ic *ImageCache) isCacheValid(filePath string) bool {
    info, err := os.Stat(filePath)
    if err != nil {
        return false
    }
    
    // 检查文件是否在有效期内
    return time.Since(info.ModTime()) < ic.cacheDuration
}

// 下载图片到缓存
func (ic *ImageCache) downloadImage(url, filePath string) error {
    // 创建带有防盗链头部的请求
    req, err := http.NewRequest("GET", url, nil)
    if err != nil {
        return fmt.Errorf("create request failed: %w", err)
    }
    
    // 🔑 关键：设置请求头绕过B站防盗链
    req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
    req.Header.Set("Referer", "https://www.bilibili.com/")
    req.Header.Set("Accept", "image/webp,image/apng,image/*,*/*;q=0.8")
    req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
    req.Header.Set("Cache-Control", "no-cache")
    req.Header.Set("Sec-Fetch-Dest", "image")
    req.Header.Set("Sec-Fetch-Mode", "no-cors")
    req.Header.Set("Sec-Fetch-Site", "cross-site")
    
    client := &http.Client{
        Timeout: 15 * time.Second,
        Transport: &http.Transport{
            MaxIdleConns:       10,
            IdleConnTimeout:    30 * time.Second,
        },
    }
    
    resp, err := client.Do(req)
    if err != nil {
        return fmt.Errorf("request failed: %w", err)
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("bad status code: %d", resp.StatusCode)
    }
    
    // 创建临时文件，避免部分下载的文件被使用
    tempPath := filePath + ".tmp"
    file, err := os.Create(tempPath)
    if err != nil {
        return fmt.Errorf("create temp file failed: %w", err)
    }
    
    // 下载图片内容
    _, err = io.Copy(file, resp.Body)
    file.Close()
    
    if err != nil {
        os.Remove(tempPath) // 清理失败的临时文件
        return fmt.Errorf("download failed: %w", err)
    }
    
    // 原子性移动文件
    if err := os.Rename(tempPath, filePath); err != nil {
        os.Remove(tempPath)
        return fmt.Errorf("move temp file failed: %w", err)
    }
    
    slog.Info("Image cached successfully", "url", url, "path", filePath)
    return nil
}

// 获取缓存的图片URL（同步版本）
func (ic *ImageCache) GetCachedImageURL(originalURL string) string {
    if originalURL == "" {
        return ""
    }
    
    // 确保使用 HTTPS
    if strings.HasPrefix(originalURL, "http://") {
        originalURL = strings.Replace(originalURL, "http://", "https://", 1)
    }
    
    filePath := ic.getCacheFilePath(originalURL)
    fileName := ic.getCacheFileName(originalURL)
    
    // 如果缓存有效，直接返回缓存URL
    if ic.isCacheValid(filePath) {
        return "/cache/images/" + fileName
    }
    
    // 防止同一图片重复下载
    ic.mutex.Lock()
    if ch, exists := ic.downloading[originalURL]; exists {
        ic.mutex.Unlock()
        // 等待其他goroutine下载完成
        <-ch
        if ic.isCacheValid(filePath) {
            return "/cache/images/" + fileName
        }
    } else {
        // 标记正在下载
        ch := make(chan struct{})
        ic.downloading[originalURL] = ch
        ic.mutex.Unlock()
        
        // 下载图片
        go func() {
            defer func() {
                close(ch)
                ic.mutex.Lock()
                delete(ic.downloading, originalURL)
                ic.mutex.Unlock()
            }()
            
            if err := ic.downloadImage(originalURL, filePath); err != nil {
                slog.Error("Failed to download image", "url", originalURL, "error", err)
            }
        }()
    }
    
    // 检查是否存在旧缓存（即使过期也先用着）
    if _, err := os.Stat(filePath); err == nil {
        return "/cache/images/" + fileName
    }
    
    // 如果没有缓存，返回原始URL作为后备
    return originalURL
}

// 预加载图片到缓存（异步版本）
func (ic *ImageCache) PreloadImage(originalURL string) {
    if originalURL == "" {
        return
    }
    
    // 确保使用 HTTPS
    if strings.HasPrefix(originalURL, "http://") {
        originalURL = strings.Replace(originalURL, "http://", "https://", 1)
    }
    
    filePath := ic.getCacheFilePath(originalURL)
    
    // 如果已经缓存且有效，跳过
    if ic.isCacheValid(filePath) {
        return
    }
    
    // 防止重复下载
    ic.mutex.Lock()
    if _, exists := ic.downloading[originalURL]; exists {
        ic.mutex.Unlock()
        return
    }
    
    ch := make(chan struct{})
    ic.downloading[originalURL] = ch
    ic.mutex.Unlock()
    
    // 异步下载
    go func() {
        defer func() {
            close(ch)
            ic.mutex.Lock()
            delete(ic.downloading, originalURL)
            ic.mutex.Unlock()
        }()
        
        if err := ic.downloadImage(originalURL, filePath); err != nil {
            slog.Error("Failed to preload image", "url", originalURL, "error", err)
        }
    }()
}

// 清理过期缓存
func (ic *ImageCache) CleanExpiredCache() {
    files, err := filepath.Glob(filepath.Join(ic.cacheDir, "*"))
    if err != nil {
        slog.Error("Failed to list cache files", "error", err)
        return
    }
    
    var cleaned int
    var totalSize int64
    
    for _, file := range files {
        info, err := os.Stat(file)
        if err != nil {
            continue
        }
        
        // 删除过期文件
        if time.Since(info.ModTime()) > ic.cacheDuration {
            if err := os.Remove(file); err == nil {
                cleaned++
                totalSize += info.Size()
            }
        }
    }
    
    if cleaned > 0 {
        slog.Info("Cache cleanup completed", 
            "files_removed", cleaned, 
            "space_freed", fmt.Sprintf("%.2fMB", float64(totalSize)/(1024*1024)))
    }
}

// 全局图片缓存实例
var globalImageCache = NewImageCache("/root/glance/glance-main/cache/images", 24*time.Hour)

func (widget *videosWidget) initialize() error {
	widget.withTitle("Videos").withCacheDuration(time.Hour)

	if widget.Limit <= 0 {
		widget.Limit = 25
	}

	if widget.CollapseAfterRows == 0 || widget.CollapseAfterRows < -1 {
		widget.CollapseAfterRows = 4
	}

	if widget.CollapseAfter == 0 || widget.CollapseAfter < -1 {
		widget.CollapseAfter = 7
	}

	// A bit cheeky, but from a user's perspective it makes more sense when channels and
	// playlists are separate things rather than specifying a list of channels and some of
	// them awkwardly have a "playlist:" prefix
	if len(widget.Playlists) > 0 {
		initialLen := len(widget.Channels)
		widget.Channels = append(widget.Channels, make([]string, len(widget.Playlists))...)

		for i := range widget.Playlists {
			widget.Channels[initialLen+i] = videosWidgetPlaylistPrefix + widget.Playlists[i]
		}
	}

	return nil
}

func (widget *videosWidget) update(ctx context.Context) {
	videos, err := fetchYoutubeChannelUploads(widget.Channels, widget.VideoUrlTemplate, widget.IncludeShorts)

	if !widget.canContinueUpdateAfterHandlingErr(err) {
		return
	}

	if len(videos) > widget.Limit {
		videos = videos[:widget.Limit]
	}

	widget.Videos = videos
}

func (widget *videosWidget) Render() template.HTML {
	var template *template.Template

	switch widget.Style {
	case "grid-cards":
		template = videosWidgetGridTemplate
	case "vertical-list":
		template = videosWidgetVerticalListTemplate
	default:
		template = videosWidgetTemplate
	}

	return widget.renderTemplate(widget, template)
}

type youtubeFeedResponseXml struct {
	Channel     string `xml:"author>name"`
	ChannelLink string `xml:"author>uri"`
	Videos      []struct {
		Title     string `xml:"title"`
		Published string `xml:"published"`
		Link      struct {
			Href string `xml:"href,attr"`
		} `xml:"link"`

		Group struct {
			Thumbnail struct {
				Url string `xml:"url,attr"`
			} `xml:"http://search.yahoo.com/mrss/ thumbnail"`
		} `xml:"http://search.yahoo.com/mrss/ group"`
	} `xml:"entry"`
}

func parseYoutubeFeedTime(t string) time.Time {
	parsedTime, err := time.Parse("2006-01-02T15:04:05-07:00", t)
	if err != nil {
		return time.Now()
	}

	return parsedTime
}

type video struct {
	ThumbnailUrl string
	Title        string
	Url          string
	Author       string
	AuthorUrl    string
	TimePosted   time.Time
	Cover        string
	Ctime        int64
	Bvid         string
}


type videoList []video

func (v videoList) sortByNewest() videoList {
	sort.Slice(v, func(i, j int) bool {
		return v[i].TimePosted.After(v[j].TimePosted)
	})

	return v
}

// func fetchYoutubeChannelUploads(channelOrPlaylistIDs []string, videoUrlTemplate string, includeShorts bool) (videoList, error) {
// 	requests := make([]*http.Request, 0, len(channelOrPlaylistIDs))

// 	for i := range channelOrPlaylistIDs {
// 		var feedUrl string
// 		if strings.HasPrefix(channelOrPlaylistIDs[i], videosWidgetPlaylistPrefix) {
// 			feedUrl = "https://www.youtube.com/feeds/videos.xml?playlist_id=" +
// 				strings.TrimPrefix(channelOrPlaylistIDs[i], videosWidgetPlaylistPrefix)
// 		} else if !includeShorts && strings.HasPrefix(channelOrPlaylistIDs[i], "UC") {
// 			playlistId := strings.Replace(channelOrPlaylistIDs[i], "UC", "UULF", 1)
// 			feedUrl = "https://www.youtube.com/feeds/videos.xml?playlist_id=" + playlistId
// 		} else {
// 			feedUrl = "https://www.youtube.com/feeds/videos.xml?channel_id=" + channelOrPlaylistIDs[i]
// 		}

// 		request, _ := http.NewRequest("GET", feedUrl, nil)
// 		requests = append(requests, request)
// 	}

// 	job := newJob(decodeXmlFromRequestTask[youtubeFeedResponseXml](defaultHTTPClient), requests).withWorkers(30)
// 	responses, errs, err := workerPoolDo(job)
// 	if err != nil {
// 		return nil, fmt.Errorf("%w: %v", errNoContent, err)
// 	}

// 	videos := make(videoList, 0, len(channelOrPlaylistIDs)*15)
// 	var failed int

// 	for i := range responses {
// 		if errs[i] != nil {
// 			failed++
// 			slog.Error("Failed to fetch youtube feed", "channel", channelOrPlaylistIDs[i], "error", errs[i])
// 			continue
// 		}

// 		response := responses[i]

// 		for j := range response.Videos {
// 			v := &response.Videos[j]
// 			var videoUrl string

// 			if videoUrlTemplate == "" {
// 				videoUrl = v.Link.Href
// 			} else {
// 				parsedUrl, err := url.Parse(v.Link.Href)

// 				if err == nil {
// 					videoUrl = strings.ReplaceAll(videoUrlTemplate, "{VIDEO-ID}", parsedUrl.Query().Get("v"))
// 				} else {
// 					videoUrl = "#"
// 				}
// 			}

// 			videos = append(videos, video{
// 				ThumbnailUrl: v.Group.Thumbnail.Url,
// 				Title:        v.Title,
// 				Url:          videoUrl,
// 				Author:       response.Channel,
// 				AuthorUrl:    response.ChannelLink + "/videos",
// 				TimePosted:   parseYoutubeFeedTime(v.Published),
// 			})
// 		}
// 	}
func fetchYoutubeChannelUploads(channelOrPlaylistIDs []string, videoUrlTemplate string, includeShorts bool) (videoList, error) {
	requests := make([]*http.Request, 0, len(channelOrPlaylistIDs))
	u := "https://app.bilibili.com/x/v2/space/archive/cursor?vmid="
	for i := range channelOrPlaylistIDs {
		request, _ := http.NewRequest("GET", u+channelOrPlaylistIDs[i], nil)
		request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
		request.Header.Set("Referer", "https://www.bilibili.com/")

		requests = append(requests, request)
	}

	job := newJob(decodeJsonFromRequestTask[bilibiliSpaceResponseJson](defaultHTTPClient), requests).withWorkers(30)

	responses, errs, err := workerPoolDo(job)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoContent, err)
	}

	videos := make(videoList, 0, len(channelOrPlaylistIDs)*15)
	var failed int
	for i := range responses {
		if errs[i] != nil {
			failed++
			slog.Error("Failed to fetch bilibili feed", "uid", channelOrPlaylistIDs[i], "error", errs[i])
			continue
		}
		response := responses[i]
		for j := range response.Data.Item {
			bilivideo := &response.Data.Item[j]
			videoUrl := `https://www.bilibili.com/video/` + bilivideo.Bvid

			// 🎯 核心修改：使用真正的缓存机制
            // cachedImageURL := globalImageCache.GetCachedImageURL(bilivideo.Cover)
            
            // // 预加载图片（可选，提升用户体验）
            // globalImageCache.PreloadImage(bilivideo.Cover)
            
            // fmt.Printf("Original cover: %s\n", bilivideo.Cover)
            // fmt.Printf("Cached cover: %s\n", cachedImageURL)

			videos = append(videos, video{
				ThumbnailUrl: bilivideo.Cover,
				// ThumbnailUrl: cachedImageURL,
				Title:        bilivideo.Title,
				Url:          strings.ReplaceAll(videoUrl, "http://", "https://"),
				Author:       bilivideo.Author,
				AuthorUrl:    `https://space.bilibili.com/` + channelOrPlaylistIDs[i],
				TimePosted:   time.Unix(bilivideo.Ctime, 0),
			})
		}
	}

	if len(videos) == 0 {
		return nil, errNoContent
	}

	videos.sortByNewest()

	if failed > 0 {
		return videos, fmt.Errorf("%w: missing videos from %d channels", errPartialContent, failed)
	}

	return videos, nil
}
