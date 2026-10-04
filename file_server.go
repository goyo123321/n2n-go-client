package main

import (
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "path/filepath"
    "sort"
    "strconv"
    "strings"
    "time"

    "golang.org/x/net/webdav"
)

var ShareServerPort = 9090

func initShareServerPort() {
    v := os.Getenv("SHARE_PORT")
    if v == "" {
        log.Printf("[共享盘] 使用默认端口 %d", ShareServerPort)
        return
    }
    n, err := strconv.Atoi(v)
    if err != nil || n < 1 || n > 65535 {
        log.Printf("[共享盘] SHARE_PORT=%q 无效，使用默认端口 %d", v, ShareServerPort)
        return
    }
    ShareServerPort = n
    log.Printf("[共享盘] 端口已覆盖为 %d", ShareServerPort)
}

type ShareServer struct {
    rootDir   string
    virtualIP string
    server    *http.Server
    nodeName  string
}

func NewShareServer(rootDir, virtualIP, nodeName string) *ShareServer {
    return &ShareServer{rootDir: rootDir, virtualIP: virtualIP, nodeName: nodeName}
}

func (s *ShareServer) Start() error {
    if err := os.MkdirAll(s.rootDir, 0755); err != nil {
        return fmt.Errorf("create share dir: %w", err)
    }

    mux := http.NewServeMux()
    davHandler := &webdav.Handler{
        Prefix:     "/webdav",
        FileSystem: webdav.Dir(s.rootDir),
        LockSystem: webdav.NewMemLS(),
    }
    mux.Handle("/webdav/", davHandler)
    mux.HandleFunc("/", s.handleIndex)
    mux.HandleFunc("/api/list", s.handleList)
    mux.HandleFunc("/api/download", s.handleDownload)
    mux.HandleFunc("/api/upload", s.handleUpload)
    mux.HandleFunc("/api/delete", s.handleDelete)
    mux.HandleFunc("/api/mkdir", s.handleMkdir)
    mux.HandleFunc("/api/node_info", s.handleNodeInfo)

    addr := fmt.Sprintf("%s:%d", s.virtualIP, ShareServerPort)
    s.server = &http.Server{
        Addr:         addr,
        Handler:      mux,
        ReadTimeout:  30 * time.Minute,
        WriteTimeout: 30 * time.Minute,
    }

    log.Printf("[共享盘] 监听 %s，目录: %s", addr, s.rootDir)
    go func() {
        if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Printf("[共享盘] 启动失败: %v", err)
        }
    }()
    return nil
}

func (s *ShareServer) Stop() error {
    if s.server != nil {
        return s.server.Close()
    }
    return nil
}

func (s *ShareServer) safePath(rel string) (string, error) {
    clean := filepath.Clean("/" + rel)
    if strings.Contains(clean, "..") {
        return "", fmt.Errorf("invalid path")
    }
    full := filepath.Join(s.rootDir, clean)
    absRoot, _ := filepath.Abs(s.rootDir)
    absFull, _ := filepath.Abs(full)
    if !strings.HasPrefix(absFull, absRoot) {
        return "", fmt.Errorf("path escape")
    }
    return full, nil
}

type fileEntry struct {
    Name    string `json:"name"`
    Size    int64  `json:"size"`
    ModTime int64  `json:"modTime"`
    IsDir   bool   `json:"isDir"`
}

func (s *ShareServer) handleList(w http.ResponseWriter, r *http.Request) {
    rel := r.URL.Query().Get("path")
    full, err := s.safePath(rel)
    if err != nil {
        http.Error(w, err.Error(), 400)
        return
    }

    entries, err := os.ReadDir(full)
    if err != nil {
        http.Error(w, err.Error(), 500)
        return
    }

    var files []fileEntry
    for _, e := range entries {
        info, err := e.Info()
        if err != nil {
            continue
        }
        files = append(files, fileEntry{
            Name: e.Name(), Size: info.Size(),
            ModTime: info.ModTime().UnixMilli(), IsDir: e.IsDir(),
        })
    }
    sort.Slice(files, func(i, j int) bool {
        if files[i].IsDir != files[j].IsDir {
            return files[i].IsDir
        }
        return files[i].Name < files[j].Name
    })
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(files)
}

func (s *ShareServer) handleDownload(w http.ResponseWriter, r *http.Request) {
    rel := r.URL.Query().Get("path")
    full, err := s.safePath(rel)
    if err != nil {
        http.Error(w, err.Error(), 400)
        return
    }
    info, err := os.Stat(full)
    if err != nil || info.IsDir() {
        http.Error(w, "not found", 404)
        return
    }
    w.Header().Set("Content-Disposition",
        fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(full)))
    http.ServeFile(w, r, full)
}

func (s *ShareServer) handleUpload(w http.ResponseWriter, r *http.Request) {
    if r.Method != "POST" {
        http.Error(w, "method not allowed", 405)
        return
    }
    rel := r.URL.Query().Get("path")
    full, err := s.safePath(rel)
    if err != nil {
        http.Error(w, err.Error(), 400)
        return
    }
    _ = os.MkdirAll(filepath.Dir(full), 0755)
    out, err := os.Create(full)
    if err != nil {
        http.Error(w, err.Error(), 500)
        return
    }
    defer out.Close()
    n, err := io.Copy(out, r.Body)
    if err != nil {
        http.Error(w, err.Error(), 500)
        return
    }
    log.Printf("[共享盘] 上传: %s (%d bytes)", full, n)
    fmt.Fprintf(w, "OK: %d bytes", n)
}

func (s *ShareServer) handleDelete(w http.ResponseWriter, r *http.Request) {
    if r.Method != "DELETE" {
        http.Error(w, "method not allowed", 405)
        return
    }
    rel := r.URL.Query().Get("path")
    full, err := s.safePath(rel)
    if err != nil {
        http.Error(w, err.Error(), 400)
        return
    }
    _ = os.RemoveAll(full)
    w.WriteHeader(200)
}

func (s *ShareServer) handleMkdir(w http.ResponseWriter, r *http.Request) {
    if r.Method != "POST" {
        http.Error(w, "method not allowed", 405)
        return
    }
    rel := r.URL.Query().Get("path")
    full, err := s.safePath(rel)
    if err != nil {
        http.Error(w, err.Error(), 400)
        return
    }
    _ = os.MkdirAll(full, 0755)
    w.WriteHeader(200)
}

func (s *ShareServer) handleNodeInfo(w http.ResponseWriter, r *http.Request) {
    info := map[string]interface{}{
        "name":      s.nodeName,
        "virtualIp": s.virtualIP,
        "port":      ShareServerPort,
        "webdav":    fmt.Sprintf("http://%s:%d/webdav/", s.virtualIP, ShareServerPort),
    }
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(info)
}

func (s *ShareServer) handleIndex(w http.ResponseWriter, r *http.Request) {
    if r.URL.Path != "/" {
        http.NotFound(w, r)
        return
    }
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Write([]byte(shareIndexHTML))
}

const shareIndexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head><meta charset="UTF-8"><title>共享盘</title>
<style>
body{font-family:system-ui,sans-serif;background:#0f172a;color:#e2e8f0;padding:24px}
h1{font-size:20px;margin-bottom:16px}.toolbar{display:flex;gap:12px;margin-bottom:16px}
button,.upload-btn{background:#3b82f6;color:#fff;border:none;padding:8px 16px;border-radius:6px;cursor:pointer}
.upload-btn input{display:none}table{width:100%;border-collapse:collapse;background:#1e293b;border-radius:8px}
th,td{padding:12px 16px;text-align:left;border-bottom:1px solid #334155}
th{background:#0f172a;color:#94a3b8;font-size:12px}a{color:#60a5fa;text-decoration:none}
.size{color:#94a3b8;font-family:monospace}
</style></head><body>
<h1>📁 共享盘</h1>
<div class="toolbar">
<label class="upload-btn">⬆️ 上传文件<input type="file" id="fileInput" multiple></label>
<button onclick="loadList()">🔄 刷新</button></div>
<table><thead><tr><th>名称</th><th>大小</th><th>修改时间</th><th>操作</th></tr></thead>
<tbody id="fileList"></tbody></table>
<script>
let currentPath='';
function fmtSize(b){if(b<1024)return b+' B';if(b<1048576)return(b/1024).toFixed(1)+' KB';if(b<1073741824)return(b/1048576).toFixed(1)+' MB';return(b/1073741824).toFixed(2)+' GB'}
function esc(s){return String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
async function loadList(){const r=await fetch('/api/list?path='+encodeURIComponent(currentPath));const files=await r.json();const t=document.getElementById('fileList');if(!files.length){t.innerHTML='<tr><td colspan="4" style="text-align:center;color:#64748b;padding:32px">此目录为空</td></tr>';return}
t.innerHTML=files.map(f=>{const i=f.isDir?'📁':'📄';const n=f.isDir?'<a onclick="navTo(\''+esc((currentPath+'/'+f.name).replace(/\/+/g,'/'))+'\')">'+i+' '+esc(f.name)+'</a>':'<a href="/api/download?path='+encodeURIComponent(currentPath+'/'+f.name)+'">'+i+' '+esc(f.name)+'</a>';return'<tr><td>'+n+'</td><td class="size">'+(f.isDir?'--':fmtSize(f.size))+'</td><td class="size">'+new Date(f.modTime).toLocaleString()+'</td><td><button style="background:#dc2626;padding:4px 12px;font-size:12px" onclick="del(\''+esc(f.name)+'\')">删除</button></td></tr>'}).join('')}
function navTo(p){currentPath=p;loadList()}
async function del(name){if(!confirm('确定删除？'))return;const fp=(currentPath+'/'+name).replace(/\/+/g,'/');await fetch('/api/delete?path='+encodeURIComponent(fp),{method:'DELETE'});loadList()}
document.getElementById('fileInput').addEventListener('change',async(e)=>{for(const f of e.target.files){await upload(f)}loadList();e.target.value=''});
function upload(file){return new Promise((res,rej)=>{const x=new XMLHttpRequest();x.onload=()=>x.status===200?res():rej();x.onerror=()=>rej();const fp=(currentPath+'/'+file.name).replace(/\/+/g,'/');x.open('POST','/api/upload?path='+encodeURIComponent(fp));x.send(file)})}
loadList();
</script></body></html>`
