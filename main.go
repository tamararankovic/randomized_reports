package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid"
	"github.com/tamararankovic/randomized_reports/config"
	"github.com/tamararankovic/randomized_reports/peers"
)

const RR_REQ_MSG_TYPE int8 = 1
const RR_RESP_MSG_TYPE int8 = 2
const RR_RESULT_MSG_TYPE int8 = 3

var root *peers.Peer

type Msg interface {
	Type() int8
}

func MsgToBytes(msg Msg) []byte {
	msgBytes, _ := json.Marshal(&msg)
	return append([]byte{byte(msg.Type())}, msgBytes...)
}

func BytesToMsg(msgBytes []byte) Msg {
	msgType := int8(msgBytes[0])
	var msg Msg
	switch msgType {
	case RR_REQ_MSG_TYPE:
		msg = &RRRequest{}
	case RR_RESP_MSG_TYPE:
		msg = &RRResponse{}
	case RR_RESULT_MSG_TYPE:
		msg = &RRResult{}
	}
	if msg == nil {
		return nil
	}
	json.Unmarshal(msgBytes[1:], msg)
	return msg
}

type RRRequest struct {
	ReqID           string
	RespProbability float64
	RespIP          string
	RespPort        int
	ReqTime         int64
}

func (m RRRequest) Type() int8 {
	return RR_REQ_MSG_TYPE
}

type RRResponse struct {
	NodeID          string
	ReqID           string
	RespProbability float64
	ReqTime         int64
	Value           float64
}

func (m RRResponse) Type() int8 {
	return RR_RESP_MSG_TYPE
}

type RRResult struct {
	Time            int64
	Sum, Count, Avg float64
}

func (m RRResult) Type() int8 {
	return RR_RESULT_MSG_TYPE
}

type Node struct {
	ID                string
	IP                string
	Port              int
	TAgg              int
	Value             float64
	RespProbability   float64
	WaitRespNs        int
	Responses         map[string][]RRResponse
	LastResp          map[string]int64
	ProcessedRequests []string
	Iteration         int
	EpochLength       int
	FirstRoundWaitSec int
	LastResult        RRResult
	Peers             *peers.Peers
	Lock              *sync.Mutex
}

func (n *Node) sendRquests() {
	for range time.NewTicker(time.Duration(n.TAgg) * time.Second).C {
		n.Iteration++
		id, err := uuid.NewV4()
		reqId := ""
		if err != nil {
			log.Println(err)
		} else {
			reqId = id.String()
		}
		req := RRRequest{
			ReqID:           reqId,
			RespIP:          n.IP,
			RespPort:        n.Port,
			RespProbability: n.RespProbability,
			ReqTime:         time.Now().UnixNano(),
		}
		msg := MsgToBytes(req)
		n.Lock.Lock()
		for _, peer := range n.Peers.GetPeers() {
			peer.Send(msg)
		}
		wait := n.WaitRespNs
		if n.Iteration%n.EpochLength == 0 {
			wait = n.FirstRoundWaitSec * 1000000000
		}
		n.Lock.Unlock()
		go func() {
			time.Sleep(time.Duration(wait) * time.Nanosecond)
			n.aggregateResponses(reqId)
		}()
	}
}

func (n *Node) aggregateResponses(reqId string) {
	n.Lock.Lock()
	defer n.Lock.Unlock()
	var sum float64 = 0
	var count float64 = 0
	var probability float64 = 1
	for _, resp := range n.Responses[reqId] {
		sum += resp.Value
		count += 1
		probability = resp.RespProbability
	}
	delete(n.Responses, reqId)
	timeElapsed := n.LastResp[reqId]
	delete(n.LastResp, reqId)
	if n.WaitRespNs < int(timeElapsed) {
		n.WaitRespNs = int(timeElapsed)
		log.Println("new wait", n.WaitRespNs)
	}
	result := RRResult{
		Time:  time.Now().UnixNano(),
		Sum:   sum / probability,
		Count: count / probability,
	}
	if result.Count > 0 {
		result.Avg = result.Sum / result.Count
	}
	n.LastResult = result
	log.Println("estimated sum", result.Sum)
	log.Println("estimated count", result.Count)
	log.Println("estimated avg", result.Avg)
	msg := MsgToBytes(result)
	for _, peer := range n.Peers.GetPeers() {
		if rand.Float64() < 0.5 {
			continue
		}
		peer.Send(msg)
	}
}

func (n *Node) onReq(req *RRRequest) {
	n.Lock.Lock()
	defer n.Lock.Unlock()
	if slices.ContainsFunc(n.ProcessedRequests, func(id string) bool {
		return id == req.ReqID
	}) {
		return
	}
	forward := MsgToBytes(req)
	for _, peer := range n.Peers.GetPeers() {
		peer.Send(forward)
	}
	n.ProcessedRequests = append(n.ProcessedRequests, req.ReqID)
	if rand.Float64() > req.RespProbability {
		return
	}
	resp := RRResponse{
		ReqID:           req.ReqID,
		NodeID:          n.ID,
		RespProbability: req.RespProbability,
		ReqTime:         req.ReqTime,
		Value:           n.Value,
	}
	msg := MsgToBytes(resp)
	var conn *peers.Peer
	if root != nil {
		conn = root
	} else {
		conn = n.Peers.NewPeer("root", req.RespIP, req.RespPort)
		root = conn
	}
	conn.Send(msg)
}

func (n *Node) onResp(resp *RRResponse) {
	n.Lock.Lock()
	defer n.Lock.Unlock()
	responses := n.Responses[resp.ReqID]
	if slices.ContainsFunc(responses, func(r RRResponse) bool {
		return r.NodeID == resp.NodeID
	}) {
		return
	}
	responses = append(responses, *resp)
	n.Responses[resp.ReqID] = responses
	n.LastResp[resp.ReqID] = time.Now().UnixNano() - resp.ReqTime
}

func (n *Node) onResult(msg *RRResult) {
	n.Lock.Lock()
	defer n.Lock.Unlock()
	if msg.Time <= n.LastResult.Time {
		// already seen
		return
	}
	n.LastResult = *msg
	log.Println("estimated sum", n.LastResult.Sum)
	log.Println("estimated count", n.LastResult.Count)
	log.Println("estimated avg", n.LastResult.Avg)
	forward := MsgToBytes(msg)
	for _, peer := range n.Peers.GetPeers() {
		if rand.Float64() < 0.5 {
			continue
		}
		peer.Send(forward)
	}
}

func main() {
	time.Sleep(10 * time.Second)

	cfg := config.LoadConfigFromEnv()
	params := config.LoadParamsFromEnv()

	ps, err := peers.NewPeers(cfg)
	if err != nil {
		log.Fatal(err)
	}

	val, err := strconv.Atoi(params.ID)
	if err != nil {
		log.Fatal(err)
	}

	node := &Node{
		ID:                params.ID,
		IP:                cfg.ListenIP,
		Port:              cfg.ListenPort,
		TAgg:              params.Tagg,
		Value:             float64(val),
		RespProbability:   params.RespProbability,
		Responses:         make(map[string][]RRResponse),
		LastResp:          make(map[string]int64),
		EpochLength:       params.EpochLength,
		FirstRoundWaitSec: params.FirstRoundWaitSec,
		Peers:             ps,
		Lock:              &sync.Mutex{},
	}

	lastRcvd := make(map[string]int)
	round := 0

	// handle messages
	go func() {
		for msgRcvd := range ps.Messages {
			msg := BytesToMsg(msgRcvd.MsgBytes)
			if msg == nil {
				continue
			}
			switch msg.Type() {
			case RR_REQ_MSG_TYPE:
				node.onReq(msg.(*RRRequest))
			case RR_RESP_MSG_TYPE:
				node.onResp(msg.(*RRResponse))
			case RR_RESULT_MSG_TYPE:
				node.onResult(msg.(*RRResult))
			}
		}
	}()

	// remove failed peers
	go func() {
		for range time.NewTicker(time.Second).C {
			round++
			for _, peer := range ps.GetPeers() {
				if lastRcvd[peer.GetID()]+params.Rmax < round && round > 10 {
					ps.PeerFailed(peer.GetID())
				}
			}
		}
	}()

	go func() {
		for range time.NewTicker(time.Second).C {
			node.exportMsgCount()
			node.Lock.Lock()
			value := node.LastResult.Avg
			node.Lock.Unlock()
			node.exportResult(value, 0, time.Now().UnixNano())
		}
	}()

	if node.ID == "1" {
		go node.sendRquests()
	}

	r := http.NewServeMux()
	r.HandleFunc("POST /metrics", node.setMetricsHandler)
	log.Println("Metrics server listening")

	log.Fatal(http.ListenAndServe(cfg.ListenIP+":9200", r))
}

func (n *Node) setMetricsHandler(w http.ResponseWriter, r *http.Request) {
	newMetrics, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()
	lines := strings.Split(string(newMetrics), "\n")
	valStr := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "app_memory_usage_bytes") {
			valStr = strings.Split(line, " ")[1]
			break
		}
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		log.Println(err)
	} else {
		log.Println("new value", val)
		n.Value = val
	}
	w.WriteHeader(http.StatusOK)
}

var writers map[string]*csv.Writer = map[string]*csv.Writer{}

func (n *Node) exportResult(value float64, reqTimestamp, rcvTimestamp int64) {
	name := "value"
	filename := fmt.Sprintf("/var/log/rand_reports/%s.csv", name)
	// defer file.Close()
	writer := writers[filename]
	if writer == nil {
		file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			log.Printf("failed to open/create file: %v", err)
			return
		}
		writer = csv.NewWriter(file)
		writers[filename] = writer
	}
	defer writer.Flush()
	reqTsStr := strconv.Itoa(int(reqTimestamp))
	rcvTsStr := strconv.Itoa(int(rcvTimestamp))
	valStr := strconv.FormatFloat(value, 'f', -1, 64)
	err := writer.Write([]string{"x", reqTsStr, rcvTsStr, valStr})
	if err != nil {
		log.Println(err)
	}
}

func (n *Node) exportMsgCount() {
	filename := "/var/log/rand_reports/msg_count.csv"
	// defer file.Close()
	writer := writers[filename]
	if writer == nil {
		file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			log.Printf("failed to open/create file: %v", err)
			return
		}
		writer = csv.NewWriter(file)
		writers[filename] = writer
	}
	defer writer.Flush()
	tsStr := strconv.Itoa(int(time.Now().UnixNano()))
	peers.MessagesSentLock.Lock()
	sent := peers.MessagesSent
	peers.MessagesSentLock.Unlock()
	peers.MessagesRcvdLock.Lock()
	rcvd := peers.MessagesRcvd
	peers.MessagesRcvdLock.Unlock()
	sentStr := strconv.Itoa(sent)
	rcvdStr := strconv.Itoa(rcvd)
	err := writer.Write([]string{tsStr, sentStr, rcvdStr})
	if err != nil {
		log.Println(err)
	}
}
