package master

import (
    "os"
    "fmt"
    "log"
    "net"
    "path"
    "sync"
    "strconv"
    "net/rpc"
    "net/http"
    "sync/atomic"

    c "github.com/Cybergenik/hopper/common"
)

type Hopper struct {
    // Havoc level to use in mutator
    havoc    int
    // seeds and Cov mutex
    mu       sync.Mutex
    // Mutation function
    mutf     func ([]byte, int) []byte
    // seed map, used as set for deduping seeds and keeping track of Crashes
    seeds    map[uint64]c.Seed
    // cov map, used as set for deduping same coverage seeds
    coverage  map[uint64]bool
    // Coverage per number of nodes
    crashes  map[string][]c.Seed
    // Max Coverage in terms of edges
    maxCov   c.Seed
    // Port to host RPC
    port     int
    // Queue Channel to add new seeds based on energy
    qChan    chan c.FTask
    // Keeps track of whether Hopper has been killed
    dead     int32
    // Node IDs
    nodes    map[int]interface{}
    //Stats
    its      int
    crashN   int
    seedsN   int
}

const (
    EXP =`
   _____ __        __      
  / ___// /_____  / /______
  \__ \/ __/ __ \/ __/ ___/
 ___/ / /_/ /_/ / /_(__  ) 
/____/\__/\__,_/\__/____/  

Havoc:          %v
Seeds:          %v
Fuzz Instances: %v
Max Edges:      %v
Crashes:        %v
UniqueCrashes:  %v
UniquePaths:    %v
Nodes:          %v

%s
`
)

func (h *Hopper) Report() {
    h.mu.Lock()
    crashes := "Crashes:\n"
    for cType, seeds := range h.crashes{
        crashes += cType + ": "
        for _, s := range seeds {
            crashes += "N"+strconv.Itoa(s.NodeId)+" "
        }
        crashes += "\n"
    }
    report := fmt.Sprintf(
        EXP,
        h.havoc,
        h.seedsN,
        h.its,
        h.maxCov.CovEdges,
        h.crashN,
        len(h.crashes),
        len(h.coverage),
        len(h.nodes),
        crashes,
    )
    out_dir := os.Getenv("HOPPER_OUT")
    var out string
    if out_dir != "" {
        out = path.Join(out_dir, "hopper.report")
    } else {
        out = "hopper.report"
    }
    os.WriteFile(out, []byte(report), 0666)
    h.mu.Unlock()
}

func (h *Hopper) Kill() {
    atomic.StoreInt32(&h.dead, 1)
}

func (h *Hopper) Stats() c.Stats{
    h.mu.Lock()
    defer h.mu.Unlock()
    return c.Stats{
        Its:           h.its,
        Port:          h.port,
        Havoc:         h.havoc,
        CrashN:        h.crashN,
        SeedsN:        h.seedsN,
        MaxSeed:       h.maxCov,
        UniqueCrashes: len(h.crashes),
        UniquePaths:   len(h.coverage),
        Nodes:         len(h.nodes),
    }
}

func (h *Hopper) killed() bool {
    z := atomic.LoadInt32(&h.dead)
    return z == 1
}

func (h *Hopper) GetFTask(args *c.FTaskArgs, task *c.FTask) error {
    t := <-h.qChan 
    t.Die = h.killed()
    *task = t
    return nil
}

func (h *Hopper) UpdateFTask(update *c.UpdateFTask, reply *c.UpdateReply) error {
    h.mu.Lock()
    defer h.mu.Unlock()
    h.nodes[update.NodeId] = nil
    // None unique or invalid seed
    if _, ok := h.seeds[update.Id]; !ok && h.seeds[update.Id].NodeId != -1 {
        return nil
    }
    // Dump Failed seeds
    if !update.Ok {
        delete(h.seeds, update.Id)
        return nil
    }
    h.its++
    h.seeds[update.Id] = c.Seed{
        NodeId:   update.NodeId,
        CovHash:  update.CovHash,
        CovEdges: update.CovEdges,
        Bytes:    h.seeds[update.Id].Bytes,
        Crash:    update.Crash != "",
    }
    // Dedup based on similar Coverage hash
    if !h.coverage[update.CovHash]{
        h.coverage[update.CovHash] = true
        // Found Unique crash, tell node to Log
        if (update.Crash != "") {
            reply.Log = true
            h.crashes[update.Crash] = append(h.crashes[update.Crash], h.seeds[update.Id])
        }
    }
    go h.energyMutate(h.seeds[update.Id], h.maxCov.CovEdges)
    if update.CovEdges > h.maxCov.CovEdges{
        h.maxCov = h.seeds[update.Id]
    }
    if (update.Crash != "") {
        h.crashN++
    }
    return nil
}

func (h *Hopper) energyMutate(seed c.Seed, maxEdges int){
    //Baseline .01% of available queue capacity
    baseline := int(float32(cap(h.qChan) - len(h.qChan)) * float32(.0005))
    mutN := 0
    covDiff := seed.CovEdges - maxEdges
    if covDiff >= 0 {
        mutN = baseline*(covDiff+1)
    } else {
        mutN = int(float32(baseline)*float32(seed.CovEdges/(maxEdges+1)))
    }
    if seed.Crash {
        mutN += baseline
    }
    for i:=0;i<mutN;i++{
        for ok := h.addSeed(h.mutf(*seed.Bytes, h.havoc)); !ok; {
            ok = h.addSeed(h.mutf(*seed.Bytes, h.havoc))
        }
    }
    seed.Bytes = nil
}

func (h *Hopper) addSeed(seed []byte) bool{
    seedHash := c.Hash(seed)
    h.mu.Lock()
    if _, ok := h.seeds[seedHash]; ok {
        h.mu.Unlock()
        return false
    }
    h.seedsN++
    h.seeds[seedHash] = c.Seed{
        NodeId:   -1,
        Bytes:    &seed,
        CovHash:  0,
        CovEdges: -1,
    }
    h.mu.Unlock()
    if len(h.qChan) == cap(h.qChan){
        go func(){
            h.qChan<-c.FTask{
                Id:       seedHash,
                Seed:     seed,
            }
        }()
    } else {
        h.qChan<-c.FTask{
            Id:       seedHash,
            Seed:     seed,
        }
    }
    return true
}

func (h *Hopper) rpcServer(){
    rpc.Register(h)
    rpc.HandleHTTP()
    config := &net.ListenConfig{
        KeepAlive: 0,
    }
    l, e := config.Listen(nil, "tcp", ":"+strconv.Itoa(h.port))
    //l, e := net.Listen("tcp", ":"+strconv.Itoa(h.port))
    if e != nil {                              
        log.Fatal("listen error:", e)
    }                                   
    go http.Serve(l, nil)                         
}

func InitHopper(havocN int, port int, mutf func([]byte, int) []byte, corpus [][]byte) *Hopper{
    h := Hopper{
        havoc:    havocN,
        mutf:     mutf,
        seeds:    make(map[uint64]c.Seed),
        coverage: make(map[uint64]bool),
        crashes:  make(map[string][]c.Seed),
        maxCov:   c.Seed{},
        port:     port,
        nodes:    make(map[int]interface{}),
        //TODO: consider using circular buffer: container/ring
        qChan:    make(chan c.FTask, 10000),
        dead:     0,
        its:      0,
        crashN:   0,
        seedsN:   0,
    }

    for _, seed := range corpus {
        h.addSeed(seed)
    }

    //Spawn RPC server
    h.rpcServer()

    return &h
}

