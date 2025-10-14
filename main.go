package main

import (
	"encoding/csv"
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

	"github.com/c12s/hyparview/data"
	"github.com/c12s/hyparview/hyparview"
	"github.com/c12s/hyparview/transport"
	"github.com/caarlos0/env"
	"github.com/gofrs/uuid"
)

const RR_REQ_MSG_TYPE data.MessageType = data.UNKNOWN + 1
const RR_RESP_MSG_TYPE data.MessageType = data.UNKNOWN + 2
const RR_RESULT_MSG_TYPE data.MessageType = data.UNKNOWN + 3

var root transport.Conn

// var estimatedAvg = 0.0

type RRRequest struct {
	ReqID           string
	RespProbability float64
	RespAddr        string
	ReqTime         int64
}

type RRResponse struct {
	NodeID          string
	ReqID           string
	RespProbability float64
	ReqTime         int64
	Value           float64
}

type RRResult struct {
	Time            int64
	Sum, Count, Avg float64
}

type Node struct {
	ID                string
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
	Hyparview         *hyparview.HyParView
	Lock              *sync.Mutex
	Logger            *log.Logger
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
			RespAddr:        n.Hyparview.Self().ListenAddress,
			RespProbability: n.RespProbability,
			ReqTime:         time.Now().UnixNano(),
		}
		msg := data.Message{
			Type:    RR_REQ_MSG_TYPE,
			Payload: req,
		}
		n.Lock.Lock()
		for _, peer := range n.Hyparview.GetPeers(1000) {
			err := peer.Conn.Send(msg)
			if err != nil {
				log.Println(err)
			}
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
		n.Logger.Println("new wait", n.WaitRespNs)
	}
	result := RRResult{
		Time:  time.Now().UnixNano(),
		Sum:   sum / probability,
		Count: count / probability,
	}
	if result.Count > 0 {
		result.Avg = result.Sum / result.Count / probability
	}
	n.LastResult = result
	n.Logger.Println("estimated sum", result.Sum)
	n.Logger.Println("estimated count", result.Count)
	n.Logger.Println("estimated avg", result.Avg)
	for _, peer := range n.Hyparview.GetPeers(1000) {
		err := peer.Conn.Send(data.Message{
			Type:    RR_RESULT_MSG_TYPE,
			Payload: result,
		})
		if err != nil {
			n.Logger.Println(err)
		}
	}
}

func (n *Node) onReq(req RRRequest) {
	// n.Logger.Println("received req", req)
	n.Lock.Lock()
	defer n.Lock.Unlock()
	if slices.ContainsFunc(n.ProcessedRequests, func(id string) bool {
		return id == req.ReqID
	}) {
		return
	}
	forward := data.Message{
		Type:    RR_REQ_MSG_TYPE,
		Payload: req,
	}
	for _, peer := range n.Hyparview.GetPeers(1000) {
		err := peer.Conn.Send(forward)
		if err != nil {
			log.Println(err)
		}
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
	msg := data.Message{
		Type:    RR_RESP_MSG_TYPE,
		Payload: resp,
	}
	// n.Logger.Println("sending resp", resp)
	var conn transport.Conn
	if root != nil {
		conn = root
	} else {
		var err error
		conn, err = transport.NewTCPConn(req.RespAddr, n.Logger)
		if err != nil {
			log.Println(err)
		} else {
			root = conn
		}
	}
	if conn == nil {
		return
	}
	err := conn.Send(msg)
	if err != nil {
		log.Println(err)
		root = nil
	} else {
		// n.Logger.Println("resp sent", resp)
	}
}

func (n *Node) onResp(resp RRResponse) {
	// n.Logger.Println("received resp", resp)
	n.Lock.Lock()
	defer n.Lock.Unlock()
	responses := n.Responses[resp.ReqID]
	if slices.ContainsFunc(responses, func(r RRResponse) bool {
		return r.NodeID == resp.NodeID
	}) {
		return
	}
	responses = append(responses, resp)
	n.Responses[resp.ReqID] = responses
	n.LastResp[resp.ReqID] = time.Now().UnixNano() - resp.ReqTime
}

func (n *Node) onResult(msg RRResult) {
	// n.Logger.Println("received resp", resp)
	n.Lock.Lock()
	defer n.Lock.Unlock()
	if msg.Time <= n.LastResult.Time {
		// already seen
		return
	}
	n.LastResult = msg
	n.Logger.Println("estimated sum", n.LastResult.Sum)
	n.Logger.Println("estimated count", n.LastResult.Count)
	n.Logger.Println("estimated avg", n.LastResult.Avg)
	forward := data.Message{
		Type:    RR_RESULT_MSG_TYPE,
		Payload: msg,
	}
	for _, peer := range n.Hyparview.GetPeers(1000) {
		err := peer.Conn.Send(forward)
		if err != nil {
			n.Logger.Println(err)
		}
	}
}

func main() {
	hvConfig := hyparview.Config{}
	err := env.Parse(&hvConfig)
	if err != nil {
		log.Fatal(err)
	}

	cfg := Config{}
	err = env.Parse(&cfg)
	if err != nil {
		log.Fatal(err)
	}

	self := data.Node{
		ID:            cfg.NodeID,
		ListenAddress: cfg.ListenAddr,
	}

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lshortfile)

	gnConnManager := transport.NewConnManager(
		transport.NewTCPConn,
		transport.AcceptTcpConnsFn(self.ListenAddress),
	)

	hv, err := hyparview.NewHyParView(hvConfig, self, gnConnManager, logger)
	if err != nil {
		log.Fatal(err)
	}
	hv.AllowAny = true

	tAgg, err := strconv.Atoi(cfg.TAgg)
	if err != nil {
		logger.Fatal(err)
	}

	val, err := strconv.Atoi(strings.Split(cfg.NodeID, "_")[2])
	if err != nil {
		logger.Fatal(err)
	}

	node := &Node{
		ID:                cfg.NodeID,
		TAgg:              tAgg,
		Value:             float64(val),
		RespProbability:   cfg.RespProbability,
		Responses:         make(map[string][]RRResponse),
		LastResp:          make(map[string]int64),
		EpochLength:       cfg.EpochLength,
		FirstRoundWaitSec: cfg.FirstRoundWaitSec,
		Hyparview:         hv,
		Lock:              &sync.Mutex{},
		Logger:            logger,
	}
	hv.AddClientMsgHandler(RR_REQ_MSG_TYPE, func(msgBytes []byte, peer hyparview.Peer) {
		msg := RRRequest{}
		err := transport.Deserialize(msgBytes, &msg)
		if err != nil {
			logger.Println(node.ID, "-", "Error unmarshaling message:", err)
			return
		}
		node.onReq(msg)
	})
	hv.AddClientMsgHandler(RR_RESP_MSG_TYPE, func(msgBytes []byte, _ hyparview.Peer) {
		msg := RRResponse{}
		err := transport.Deserialize(msgBytes, &msg)
		if err != nil {
			logger.Println(node.ID, "-", "Error unmarshaling message:", err)
			return
		}
		node.onResp(msg)
	})
	hv.AddClientMsgHandler(RR_RESULT_MSG_TYPE, func(msgBytes []byte, _ hyparview.Peer) {
		msg := RRResult{}
		err := transport.Deserialize(msgBytes, &msg)
		if err != nil {
			logger.Println(node.ID, "-", "Error unmarshaling message:", err)
			return
		}
		node.onResult(msg)
	})

	go func() {
		for range time.NewTicker(time.Second).C {
			node.exportMsgCount()
			node.Lock.Lock()
			value := node.LastResult.Avg
			node.Lock.Unlock()
			node.exportResult(value, 0, time.Now().UnixNano())
		}
	}()

	err = hv.Join(cfg.ContactID, cfg.ContactAddr)
	if err != nil {
		logger.Fatal(err)
	}

	if node.ID == "r1_node_1" {
		go node.sendRquests()
	}

	r := http.NewServeMux()
	r.HandleFunc("POST /metrics", node.setMetricsHandler)
	log.Println("Metrics server listening")

	go func() {
		log.Fatal(http.ListenAndServe(strings.Split(os.Getenv("LISTEN_ADDR"), ":")[0]+":9200", r))
	}()

	r2 := http.NewServeMux()
	r2.HandleFunc("GET /state", node.StateHandler)
	log.Println("State server listening on :5001/state")
	log.Fatal(http.ListenAndServe(strings.Split(os.Getenv("LISTEN_ADDR"), ":")[0]+":5001", r2))
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
		n.Logger.Println(err)
	} else {
		n.Logger.Println("new value", val)
		n.Value = val
	}
	w.WriteHeader(http.StatusOK)
}

var writers map[string]*csv.Writer = map[string]*csv.Writer{}

func (n *Node) exportResult(value float64, reqTimestamp, rcvTimestamp int64) {
	name := "value"
	filename := fmt.Sprintf("/var/log/fu/results/%s.csv", name)
	// defer file.Close()
	writer := writers[filename]
	if writer == nil {
		file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			n.Logger.Printf("failed to open/create file: %v", err)
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
		n.Logger.Println(err)
	}
}

func (n *Node) exportMsgCount() {
	filename := "/var/log/fu/results/msg_count.csv"
	// defer file.Close()
	writer := writers[filename]
	if writer == nil {
		file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			n.Logger.Printf("failed to open/create file: %v", err)
			return
		}
		writer = csv.NewWriter(file)
		writers[filename] = writer
	}
	defer writer.Flush()
	tsStr := strconv.Itoa(int(time.Now().UnixNano()))
	transport.MessagesSentLock.Lock()
	sent := transport.MessagesSent - transport.MessagesSentSub
	transport.MessagesSentLock.Unlock()
	transport.MessagesRcvdLock.Lock()
	rcvd := transport.MessagesRcvd - transport.MessagesRcvdSub
	transport.MessagesRcvdLock.Unlock()
	sentStr := strconv.Itoa(sent)
	rcvdStr := strconv.Itoa(rcvd)
	err := writer.Write([]string{tsStr, sentStr, rcvdStr})
	if err != nil {
		n.Logger.Println(err)
	}
}
