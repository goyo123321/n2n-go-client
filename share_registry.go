package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "sync"
    "time"
)

type ShareNode struct {
    ClientID  string `json:"clientId"`
    NodeName  string `json:"name"`
    VirtualIP string `json:"virtualIp"`
    Port      int    `json:"port"`
    Online    bool   `json:"online"`
    LastSeen  int64  `json:"lastSeen"`
}

type ShareRegistry struct {
    mu    sync.RWMutex
    nodes map[string]*ShareNode
}

func NewShareRegistry() *ShareRegistry {
    return &ShareRegistry{nodes: make(map[string]*ShareNode)}
}

func (r *ShareRegistry) Register(clientID, name, virtualIP string, port int) {
    r.mu.Lock()
    defer r.mu.Unlock()
    if port <= 0 {
        port = ShareServerPort
    }
    r.nodes[clientID] = &ShareNode{
        ClientID: clientID, NodeName: name, VirtualIP: virtualIP,
        Port: port, Online: true, LastSeen: time.Now().UnixMilli(),
    }
}

func (r *ShareRegistry) Unregister(clientID string) {
    r.mu.Lock()
    defer r.mu.Unlock()
    if n, ok := r.nodes[clientID]; ok {
        n.Online = false
    }
}

func (r *ShareRegistry) GetPort(clientID string) int {
    r.mu.RLock()
    defer r.mu.RUnlock()
    if n, ok := r.nodes[clientID]; ok && n.Port > 0 {
        return n.Port
    }
    return ShareServerPort
}

func (r *ShareRegistry) List() []*ShareNode {
    r.mu.RLock()
    defer r.mu.RUnlock()
    now := time.Now().UnixMilli()
    var out []*ShareNode
    for _, n := range r.nodes {
        if now-n.LastSeen > 30000 {
            n.Online = false
        }
        out = append(out, n)
    }
    return out
}

func (r *ShareRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
    switch req.URL.Path {
    case "/api/nodes":
        w.Header().Set("Content-Type", "application/json")
        _ = json.NewEncoder(w).Encode(r.List())
    default:
        http.NotFound(w, req)
    }
}

func DiscoverRemoteNode(virtualIP string, port int) (*ShareNode, error) {
    if port <= 0 {
        port = ShareServerPort
    }
    url := fmt.Sprintf("http://%s:%d/api/node_info", virtualIP, port)
    client := &http.Client{Timeout: 3 * time.Second}
    resp, err := client.Get(url)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    var node ShareNode
    if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
        return nil, err
    }
    if node.Port <= 0 {
        node.Port = port
    }
    node.VirtualIP = virtualIP
    node.Online = true
    node.LastSeen = time.Now().UnixMilli()
    return &node, nil
}
