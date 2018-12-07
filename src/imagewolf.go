package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/docker/distribution/notifications"
)

var seen = make(map[string]bool)
var mu sync.Mutex

var peerSet = make(map[string]bool)
var peers []net.IP
var torrentPeers []torrent.Peer //Should be ptrs IMO, but underlying lib wants copies
var torrentClient *torrent.Client
var apiPort = 8000
var dataDir = "/data"
var myIps = make(map[string]bool)
var lookupHost string

func main() {
	lookupHost = os.Getenv("LOOKUP_HOST")

	if lookupHost == "" {
		lookupHost = "tasks.imagewolf"
	}

	log.Printf("LOOKUP_HOST set to %s", lookupHost)

	var clientConfig torrent.ClientConfig
	clientConfig.DataDir = dataDir
	clientConfig.Seed = true
	clientConfig.DisableTrackers = true
	clientConfig.NoDHT = true
	clientConfig.ListenPort = 6000
	var err error
	torrentClient, err = torrent.NewClient(&clientConfig)
	if err != nil {
		log.Printf("error creating client: %s", err)
		return
	}

	http.HandleFunc("/registryNotifications", regHandler)
	http.HandleFunc("/hubNotifications", hubHandler)
	http.HandleFunc("/torrent", torrentHandler)
	http.HandleFunc("/stats", statsHandler)

	log.Println("Starting up")
	getMyIps()
	getPeers()
	//Registry expects to find us on port 8000
	log.Fatal(http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", apiPort), nil))
}

func getMyIps() {

	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("Failed to inspect my network interfaces %v\n", err)
	}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			log.Printf("Failed to inspect network addresses %v\n", err)
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			log.Printf("Found my IP %s\n", ip.String())
			myIps[ip.String()] = true
		}
	}
}

func statsHandler(w http.ResponseWriter, r *http.Request) {

	defer r.Body.Close()

	for _, tor := range torrentClient.Torrents() {
		fmt.Fprintf(w, "Torrent: %v\n", tor.Name())
		fmt.Fprintf(w, "br %v bw %v\n", tor.Stats().BytesRead, tor.Stats().BytesWritten)
	}

}

func getPeers() {

	//ips, err := net.LookupIP(lookupHost)

	ips := []string{"10.99.204.17", "10.99.204.176", "10.100.67.248", "10.100.67.249", "10.100.65.155",
	"10.100.65.156", "10.100.67.160", "10.100.56.143", "10.100.56.144"}

	//if err != nil {
	//	log.Printf("Error looking up hosts")
	//	return
	//}

	for _, ip := range ips {

		if !peerSet[ip] && !myIps[ip] {
			//log.Printf("%v %v", c, ip)

			peerSet[ip] = true
			peers = append(peers, net.ParseIP(ip).To4())
			torrentPeers = append(torrentPeers, torrent.Peer{
				IP: net.ParseIP(ip).To4(), Port: 6000})
		}
	}

}

func torrentHandler(w http.ResponseWriter, r *http.Request) {

	defer r.Body.Close()
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		log.Printf("unexpected request method: %v", r.Method)
		return
	}

	// Extract the content type and make sure it matches
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("error parsing media type: %v, contenttype=%q", err, contentType)
		return
	}

	if mediaType != "application/octet-stream" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		log.Printf("incorrect media type: %q != %q", mediaType, "application/octet-stream")
		return
	}

	var mi metainfo.MetaInfo
	dec := bencode.NewDecoder(r.Body)
	if err := dec.Decode(&mi); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("error decoding request body: %v", err)
		return
	}
	log.Printf("Got torrent, retrieving")
	seedTorrent(&mi, loadImageFromTorrent)

}

func loadImageFromTorrent(t *torrent.Torrent) {

	//should be a single file
	log.Printf("Got: %d bytes\n", t.BytesCompleted())
	log.Printf("Not got: %d bytes\n", t.BytesMissing())

	err := exec.Command("docker", "load", "-i", dataDir+"/"+t.Files()[0].Path()).Run()
	if err != nil {
		log.Printf("Failed load %v\n", err)
		return
	}
	log.Println("Loaded")

}

func regHandler(w http.ResponseWriter, r *http.Request) {

	defer r.Body.Close()
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		log.Printf("unexpected request method: %v", r.Method)
		return
	}

	// Extract the content type and make sure it matches
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("error parsing media type: %v, contenttype=%q", err, contentType)
		return
	}

	if mediaType != notifications.EventsMediaType {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		log.Printf("incorrect media type: %q != %q", mediaType, notifications.EventsMediaType)
		return
	}

	var envelope notifications.Envelope
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&envelope); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("error decoding request body: %v", err)
		return
	}

	evs := envelope.Events
	for _, e := range evs {

		mu.Lock()
		if !seen[e.ID] && e.Action == "push" &&
			e.Target.MediaType == "application/vnd.docker.distribution.manifest.v2+json" {
			seen[e.ID] = true
			logEvent(e)
			//Probably need to put this in a go func, but want to make sure not rc
			downloadAndSeedImage("localhost:5000", e.Target.Repository, e.Target.Tag)
		}
		mu.Unlock()
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK\n")

	//filter on action push and mediatype manifest?

}

func hubHandler(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		log.Printf("unexpected request method: %v", r.Method)
		return
	}

	// Extract the content type and make sure it matches
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("error parsing media type: %v, contenttype=%q", err, contentType)
		return
	}

	if mediaType != "application/json" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		log.Printf("incorrect media type: %q != %q", mediaType, "application/json")
		return
	}

	var hubEvent map[string]interface{}

	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&hubEvent); err != nil {
		log.Printf("Failed to read JSON from Hub")
		panic(err)
	}
	pd := hubEvent["push_data"].(map[string]interface{})
	tag := pd["tag"].(string)
	rep := hubEvent["repository"].(map[string]interface{})
	repoName := rep["repo_name"].(string)

	if tag != "" && repoName != "" {
		go func() {
			downloadAndSeedImage("docker.io", repoName, tag)
		}()
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Recieved notification of update to %s:%s\n", repoName, tag)

}

func downloadAndSeedImage(registry string, repo string, tag string) {

	//Could directly use API rather than docker pull
	imageName := fmt.Sprintf("%s/%s:%s", registry, repo, tag)
	err := exec.Command("docker", "pull", imageName).Run()
	if err != nil {
		log.Printf("Failed pull of %s %v\n", imageName, err)
		return

	}
	log.Println("Pulled")

	tmpfile, err := ioutil.TempFile(dataDir, strings.Replace(repo+tag, "/", "", -1))
	if err != nil {
		log.Print(err)
		return
	}
	tmpfile.Close()
	err = exec.Command("docker", "save", "-o", tmpfile.Name(), imageName).Run()
	if err != nil {
		log.Printf("Failed save %v\n", err)
		return
	}
	log.Println("Saved")
	mi := createTorrent(tmpfile)
	seedTorrent(&mi, notifyPeers)
	log.Println("Seeding")

}

func seedTorrent(mi *metainfo.MetaInfo, cb func(*torrent.Torrent)) {

	t, err := torrentClient.AddTorrent(mi)
	getPeers()

	t.AddPeers(torrentPeers)
	if err != nil {
		log.Printf("error adding torrent: %s", err)
		return
	}
	go func() {
		<-t.GotInfo()

		t.DownloadAll()

		for t.BytesMissing() > 0 {
			time.Sleep(1 * time.Second)
			log.Printf("Got: %d bytes missing %d\n", t.BytesCompleted(), t.BytesMissing())
		}
		cb(t)
	}()

}

func notifyPeers(t *torrent.Torrent) {

	getPeers()
	mi := t.Metainfo()
	data, err := bencode.Marshal(mi)
	if err != nil {
		log.Printf("Failed to create JSON %v\n", err)
	}

	for _, ip := range peers {

		url := fmt.Sprintf("http://%s:%d/torrent", ip.String(), apiPort)
		log.Printf("Notifying: %s\n", url)

		_, err := http.Post(url, "application/octet-stream", bytes.NewReader(data))
		if err != nil {
			log.Printf("notify responded with err %v\n", err)
		}
	}

}

func createTorrent(f *os.File) metainfo.MetaInfo {
	mi := metainfo.MetaInfo{}

	mi.SetDefaults()
	info := metainfo.Info{
		PieceLength: 256 * 1024,
	}

	err := info.BuildFromFilePath(f.Name())
	if err != nil {
		log.Fatal(err)
	}
	mi.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		log.Fatal(err)
	}

	return mi

}

func logEvent(e notifications.Event) {

	log.Println("EVENT")
	log.Printf("ACTION: %v\n", e.Action)
	log.Printf("ACTOR: %v\n", e.Actor)
	log.Printf("ID: %v\n", e.ID)
	log.Printf("REQUEST: %v\n", e.Request)
	log.Printf("SOURCE: %v\n", e.Source)
	log.Printf("TARGET DESCRIPTOR: %v\n", e.Target.Descriptor)
	log.Printf("TARGET FROMREPO: %v\n", e.Target.FromRepository)
	log.Printf("TARGET MEDIATYPE: %v\n", e.Target.MediaType)
	log.Printf("TARGET REPO: %v\n", e.Target.Repository)
	log.Printf("TARGET TAG: %v\n", e.Target.Tag)
	log.Printf("TARGET URLs: %v\n", e.Target.URLs)
	log.Printf("TARGET URL: %v\n", e.Target.URL)
	log.Printf("TIMESTAMP: %v\n", e.Timestamp)
}
