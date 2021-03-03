package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xxf098/lite-proxy/common"
	"github.com/xxf098/lite-proxy/download"
	"github.com/xxf098/lite-proxy/request"
)

// support proxy
// concurrency setting
func getSubscriptionLinks(link string) ([]string, error) {
	c := http.Client{
		Timeout: 20 * time.Second,
	}
	resp, err := c.Get(link)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	msg, err := common.DecodeB64(string(data))
	if err != nil {
		return nil, err
	}
	return parseLinks(msg)
}

func parseLinks(message string) ([]string, error) {
	// splits := strings.SplitN(string(message), "^", 2)
	// if len(splits) < 1 {
	// 	return nil, errors.New("Invalid Data")
	// }
	matched, err := regexp.MatchString(`^(?:https?:\/\/)(?:[^@\/\n]+@)?(?:www\.)?([^:\/\n]+)`, message)
	if matched && err == nil {
		return getSubscriptionLinks(message)
	}
	reg := regexp.MustCompile(`((?i)(vmess|ssr)://[a-zA-Z0-9+_/=-]+)|((?i)(ss|trojan)://(.+?)@(.+?):([0-9]{2,5})([?#][^\s]+))`)
	matches := reg.FindAllStringSubmatch(message, -1)
	links := make([]string, len(matches))
	for index, match := range matches {
		links[index] = match[0]
	}
	if len(links) < 1 {
		return nil, errors.New("Invalid Data")
	}
	return links, nil
}

func parseOptions(message string) (*ProfileTestOptions, error) {
	opts := strings.Split(message, "^")
	if len(opts) < 6 {
		return nil, errors.New("Invalid Data")
	}
	concurrency, err := strconv.Atoi(opts[5])
	if err != nil {
		return nil, err
	}
	testOpt := &ProfileTestOptions{
		SpeedtestMode: opts[1],
		PingMethod:    opts[2],
		SortMethod:    opts[3],
		Concurrency:   concurrency,
		Timeout:       20 * time.Second,
	}
	return testOpt, nil
}

func parseMessage(message []byte) ([]string, *ProfileTestOptions, error) {
	splits := strings.SplitN(string(message), "^", 2)
	if len(splits) < 2 {
		return nil, nil, errors.New("Invalid Data")
	}
	links, err := parseLinks(splits[0])
	if err != nil {
		return nil, nil, err
	}
	options, err := parseOptions(splits[1])
	if err != nil {
		return nil, nil, err
	}
	return links, options, nil
}

type ProfileTestOptions struct {
	SpeedtestMode string
	PingMethod    string
	SortMethod    string
	Concurrency   int
	Timeout       time.Duration
}

type ProfileTest struct {
	Conn        *websocket.Conn
	Options     *ProfileTestOptions
	MessageType int
	Links       []string
	mu          sync.Mutex
	wg          sync.WaitGroup // wait for all to finish
}

func (p *ProfileTest) WriteMessage(data []byte) error {
	p.mu.Lock()
	err := p.Conn.WriteMessage(p.MessageType, data)
	p.mu.Unlock()
	return err
}

func (p *ProfileTest) WriteString(data string) error {
	b := []byte(data)
	return p.WriteMessage(b)
}

func (p *ProfileTest) testAll(ctx context.Context) error {
	if len(p.Links) < 1 {
		p.WriteString(SPEEDTEST_ERROR_NONODES)
		return fmt.Errorf("No nodes found!")
	}
	p.WriteMessage(getMsgByte(-1, "started"))
	for i, _ := range p.Links {
		p.WriteMessage(gotserverMsg(i, p.Links[i]))
	}
	guard := make(chan int, p.Options.Concurrency)
	for i, _ := range p.Links {
		p.wg.Add(1)
		select {
		case guard <- i:
			go func(index int, c <-chan int) {
				p.testOne(ctx, index)
				<-c
			}(i, guard)
		case <-ctx.Done():
			break
		}
	}
	p.wg.Wait()
	p.WriteMessage(getMsgByte(-1, "eof"))
	return nil
}

func (p *ProfileTest) testOne(ctx context.Context, index int) error {
	// panic
	defer p.wg.Done()
	p.WriteMessage(getMsgByte(index, "startping"))
	link := p.Links[index]
	link = strings.SplitN(link, "^", 2)[0]
	elapse, err := request.PingLink(link)
	err = p.WriteMessage(getMsgByte(index, "gotping", elapse))
	if elapse < 1 {
		p.WriteMessage(getMsgByte(index, "gotspeed", -1, -1))
		return err
	}
	err = p.WriteMessage(getMsgByte(index, "startspeed"))
	ch := make(chan int64, 1)
	go func(ch <-chan int64) {
		var max int64
		var speeds []int64
		for {
			select {
			case speed := <-ch:
				if speed < 0 {
					return
				}
				speeds = append(speeds, speed)
				var avg int64
				for _, s := range speeds {
					avg += s / int64(len(speeds))
				}
				if max < speed {
					max = speed
				}
				log.Printf("recv: %s", download.ByteCountIEC(speed))
				err = p.WriteMessage(getMsgByte(index, "gotspeed", avg, max))
			case <-ctx.Done():
				log.Printf("index %d done!", index)
				return
			}
		}
	}(ch)
	download.Download(link, p.Options.Timeout, p.Options.Timeout, ch)
	return err
}
