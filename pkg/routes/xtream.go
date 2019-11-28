package routes

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jamesnetherton/m3u"
	"github.com/pierre-emmanuelJ/iptv-proxy/pkg/config"
	proxyM3U "github.com/pierre-emmanuelJ/iptv-proxy/pkg/m3u"
	xtreamapi "github.com/pierre-emmanuelJ/iptv-proxy/pkg/xtream-proxy"
)

type cacheMeta struct {
	string
	time.Time
}

var hlsChannelsRedirectURL map[string]url.URL = map[string]url.URL{}
var hlsChannelsRedirectURLLock = sync.RWMutex{}

// XXX Use key/value storage e.g: etcd, redis...
// and remove that dirty globals
var xtreamM3uCache map[string]cacheMeta = map[string]cacheMeta{}
var xtreamM3uCacheLock = sync.RWMutex{}

func (p *proxy) cacheXtreamM3u(m3uURL *url.URL) error {
	playlist, err := m3u.Parse(m3uURL.String())
	if err != nil {
		return err
	}

	newM3U, err := xtreamReplaceURL(&playlist, p.User, p.Password, p.HostConfig, p.HTTPS)
	if err != nil {
		return err
	}

	result, err := proxyM3U.Marshall(newM3U)
	if err != nil {
		return err
	}

	xtreamM3uCacheLock.Lock()
	path, err := writeCacheTmp([]byte(result), m3uURL.String())
	if err != nil {
		return err
	}

	xtreamM3uCache[m3uURL.String()] = cacheMeta{path, time.Now()}
	xtreamM3uCacheLock.Unlock()

	return nil
}

func writeCacheTmp(data []byte, url string) (string, error) {
	filename := base64.StdEncoding.EncodeToString([]byte(url))
	path := filepath.Join("/tmp", filename)
	if err := ioutil.WriteFile(path, data, 0644); err != nil {
		return "", err
	}

	return path, nil
}

func (p *proxy) xtreamGetAuto(c *gin.Context) {
	newQuery := c.Request.URL.Query()
	q := p.RemoteURL.Query()
	for k, v := range q {
		if k == "username" || k == "password" {
			continue
		}

		newQuery.Add(k, strings.Join(v, ","))
	}
	c.Request.URL.RawQuery = newQuery.Encode()

	p.xtreamGet(c)
}

func (p *proxy) xtreamGet(c *gin.Context) {
	rawURL := fmt.Sprintf("%s/get.php?username=%s&password=%s", p.XtreamBaseURL, p.XtreamUser, p.XtreamPassword)

	q := c.Request.URL.Query()

	for k, v := range q {
		if k == "username" || k == "password" {
			continue
		}

		rawURL = fmt.Sprintf("%s&%s=%s", rawURL, k, strings.Join(v, ","))
	}

	println(rawURL)

	m3uURL, err := url.Parse(rawURL)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	xtreamM3uCacheLock.RLock()
	meta, ok := xtreamM3uCache[m3uURL.String()]
	d := time.Now().Sub(meta.Time)
	if !ok || d.Hours() >= float64(p.M3UCacheExpiration) {
		log.Printf("[iptv-proxy] %v | %s | xtream cache m3u file\n", time.Now().Format("2006/01/02 - 15:04:05"), c.ClientIP())
		xtreamM3uCacheLock.RUnlock()
		if err := p.cacheXtreamM3u(m3uURL); err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
	} else {
		xtreamM3uCacheLock.RUnlock()
	}

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, p.M3UFileName))
	xtreamM3uCacheLock.RLock()
	path := xtreamM3uCache[m3uURL.String()].string
	xtreamM3uCacheLock.RUnlock()
	data, err := ioutil.ReadFile(path)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", data)

}

func (p *proxy) xtreamPlayerAPIGET(c *gin.Context) {
	p.xtreamPlayerAPI(c, c.Request.URL.Query())
}

func (p *proxy) xtreamPlayerAPIPOST(c *gin.Context) {
	contents, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	q, err := url.ParseQuery(string(contents))
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	p.xtreamPlayerAPI(c, q)
}

func (p *proxy) xtreamPlayerAPI(c *gin.Context, q url.Values) {
	var action string
	if len(q["action"]) > 0 {
		action = q["action"][0]
	}

	protocol := "http"
	if p.HTTPS {
		protocol = "https"
	}

	client, err := xtreamapi.New(p.XtreamUser, p.XtreamPassword, p.XtreamBaseURL)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	var respBody interface{}

	switch action {
	case xtreamapi.GetLiveCategories:
		respBody, err = client.GetLiveCategories()
	case xtreamapi.GetLiveStreams:
		respBody, err = client.GetLiveStreams("")
	case xtreamapi.GetVodCategories:
		respBody, err = client.GetVideoOnDemandCategories()
	case xtreamapi.GetVodStreams:
		respBody, err = client.GetVideoOnDemandStreams("")
	case xtreamapi.GetVodInfo:
		if len(q["vod_id"]) < 1 {
			c.AbortWithError(http.StatusBadRequest, fmt.Errorf(`bad body url query parameters: missing "vod_id"`))
			return
		}
		respBody, err = client.GetVideoOnDemandInfo(q["vod_id"][0])
	case xtreamapi.GetSeriesCategories:
		respBody, err = client.GetSeriesCategories()
	case xtreamapi.GetSeries:
		respBody, err = client.GetSeries("")
	case xtreamapi.GetSerieInfo:
		if len(q["series_id"]) < 1 {
			c.AbortWithError(http.StatusBadRequest, fmt.Errorf(`bad body url query parameters: missing "series_id"`))
			return
		}
		respBody, err = client.GetSeriesInfo(q["series_id"][0])
	case xtreamapi.GetShortEPG:
		if len(q["stream_id"]) < 1 {
			c.AbortWithError(http.StatusBadRequest, fmt.Errorf(`bad body url query parameters: missing "stream_id"`))
			return
		}
		limit := 0
		if len(q["limit"]) > 0 {
			limit, err = strconv.Atoi(q["limit"][0])
			if err != nil {
				c.AbortWithError(http.StatusInternalServerError, err)
				return
			}
		}
		respBody, err = client.GetShortEPG(q["stream_id"][0], limit)
	case xtreamapi.GetSimpleDataTable:
		if len(q["stream_id"]) < 1 {
			c.AbortWithError(http.StatusBadRequest, fmt.Errorf(`bad body url query parameters: missing "stream_id"`))
			return
		}
		respBody, err = client.GetEPG(q["stream_id"][0])
	default:
		respBody, err = client.Login(p.User, p.Password, protocol+"://"+p.HostConfig.Hostname, int(p.HostConfig.Port), protocol)
	}

	log.Printf("[iptv-proxy] %v | %s |Action\t%s\n", time.Now().Format("2006/01/02 - 15:04:05"), c.ClientIP(), action)

	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusOK, respBody)
}

func (p *proxy) xtreamXMLTV(c *gin.Context) {
	client, err := xtreamapi.New(p.XtreamUser, p.XtreamPassword, p.XtreamBaseURL)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	resp, err := client.GetXMLTV()
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	c.Data(http.StatusOK, "application/xml", resp)
}

func (p *proxy) xtreamStream(c *gin.Context) {
	id := c.Param("id")
	rpURL, err := url.Parse(fmt.Sprintf("%s/%s/%s/%s", p.XtreamBaseURL, p.XtreamUser, p.XtreamPassword, id))
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	p.stream(c, rpURL)
}

func (p *proxy) xtreamStreamLive(c *gin.Context) {
	id := c.Param("id")
	rpURL, err := url.Parse(fmt.Sprintf("%s/live/%s/%s/%s", p.XtreamBaseURL, p.XtreamUser, p.XtreamPassword, id))
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	p.stream(c, rpURL)
}

func (p *proxy) xtreamStreamMovie(c *gin.Context) {
	id := c.Param("id")
	rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", p.XtreamBaseURL, p.XtreamUser, p.XtreamPassword, id))
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	p.stream(c, rpURL)
}

func (p *proxy) xtreamStreamSeries(c *gin.Context) {
	id := c.Param("id")
	rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", p.XtreamBaseURL, p.XtreamUser, p.XtreamPassword, id))
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	p.stream(c, rpURL)
}

func (p *proxy) hlsrStream(c *gin.Context) {
	hlsChannelsRedirectURLLock.RLock()
	url, ok := hlsChannelsRedirectURL[c.Param("channel")+".m3u8"]
	if !ok {
		c.AbortWithError(http.StatusNotFound, errors.New("HSL redirect url not found"))
		return
	}
	hlsChannelsRedirectURLLock.RUnlock()

	req, err := url.Parse(
		fmt.Sprintf(
			"%s://%s/hlsr/%s/%s/%s/%s/%s/%s",
			url.Scheme,
			url.Host,
			c.Param("token"),
			p.XtreamUser,
			p.XtreamPassword,
			c.Param("channel"),
			c.Param("hash"),
			c.Param("chunk"),
		),
	)

	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	p.stream(c, req)
}

func xtreamReplaceURL(playlist *m3u.Playlist, user, password string, hostConfig *config.HostConfiguration, https bool) (*m3u.Playlist, error) {
	result := make([]m3u.Track, 0, len(playlist.Tracks))
	for _, track := range playlist.Tracks {
		oriURL, err := url.Parse(track.URI)
		if err != nil {
			return nil, err
		}

		protocol := "http"
		if https {
			protocol = "https"
		}

		id := filepath.Base(oriURL.Path)

		uri := fmt.Sprintf(
			"%s://%s:%d/%s/%s/%s",
			protocol,
			hostConfig.Hostname,
			hostConfig.Port,
			url.QueryEscape(user),
			url.QueryEscape(password),
			url.QueryEscape(id),
		)
		destURL, err := url.Parse(uri)
		if err != nil {
			return nil, err
		}

		track.URI = destURL.String()
		result = append(result, track)
	}

	return &m3u.Playlist{
		Tracks: result,
	}, nil
}
