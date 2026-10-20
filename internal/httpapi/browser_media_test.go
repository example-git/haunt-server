package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
)

// This is a protocol/codec harness, not the product client. Chrome's actual
// encoders and decoders exchange media through the authenticated server route.
func TestBrowserWebCodecsMediaRelay(t *testing.T) {
	browser, err := exec.LookPath("google-chrome")
	if err != nil {
		browser, err = exec.LookPath("chromium")
	}
	if err != nil {
		t.Skip("Chrome/Chromium unavailable; browser media check not run")
	}
	f, _ := featureFixture(t, 256_000_000, func(c *config.Config) { c.Cloudflared = true })
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")
	var room identity.Channel
	f.call(t, "POST", "/api/v1/channels", f.owner.Token, map[string]any{"name": "browser", "kind": "voice"}, 201, &room)
	inputs, _ := json.Marshal(map[string]string{"alice": alice.Token, "bob": bob.Token, "room": room.ID})
	script := `(async () => {
const input = ` + string(inputs) + `;
const sockets = [];
function socket(path) {
  const ws = new WebSocket(location.origin.replace('http','ws')+path); ws.binaryType='arraybuffer'; sockets.push(ws);
  const queue=[],waiting=[];
  ws.onmessage = event => {
    const data=typeof event.data==='string'?JSON.parse(event.data):event.data;
    const index=waiting.findIndex(w=>w.match(data));
    if(index>=0){const w=waiting.splice(index,1)[0];clearTimeout(w.timer);w.resolve(data)}else queue.push(data);
  };
  return {ws, open:new Promise((resolve,reject)=>{ws.onopen=resolve;ws.onerror=()=>reject(Error('socket open failed'))}),
    send:data=>ws.send(JSON.stringify(data)), wait:match=>{
      const index=queue.findIndex(match);if(index>=0)return Promise.resolve(queue.splice(index,1)[0]);
      return new Promise((resolve,reject)=>{const w={match,resolve};w.timer=setTimeout(()=>reject(Error('socket response timeout')),8000);waiting.push(w)});
    }};
}
async function join(token) {
  const response=await fetch('/api/v1/voice/'+input.room+'/admissions',{method:'POST',headers:{Authorization:'Bearer '+token}});
  if(!response.ok)throw Error('admission failed');const admission=await response.json();
  if(admission.transport!=='websocket')throw Error('incorrect automatic transport');
  const events=socket('/api/v1/events');await events.open;events.send({type:'authenticate',token});await events.wait(x=>x.type==='ready');
  events.send({type:'media.join',admission_id:admission.admission_id});const {participant}=await events.wait(x=>x.type==='media.joined');
  const media=socket(admission.media_path);await media.open;media.send({type:'authenticate',token,participant_id:participant.participant_id});await media.wait(x=>x.type==='ready');
  return {events,media,id:participant.participant_id};
}
try {
  if(!globalThis.VideoEncoder||!globalThis.AudioEncoder)throw Error('WebCodecs encoders unavailable');
  const pub=await join(input.alice),sub=await join(input.bob);
  pub.events.send({type:'media.state',state:{muted:false,deafened:false,camera_off:false,screen_off:true}});
  await pub.events.wait(x=>x.type==='media.state.accepted');
  let video,audio,videoConfig,audioConfig,encodeError;
  const canvas=document.createElement('canvas');canvas.width=64;canvas.height=64;const paint=canvas.getContext('2d');paint.fillStyle='#d44a20';paint.fillRect(0,0,64,64);
  const ve=new VideoEncoder({output:(chunk,metadata)=>{video=chunk;if(metadata.decoderConfig)videoConfig=metadata.decoderConfig},error:e=>encodeError=e});
  ve.configure({codec:'vp8',width:64,height:64,bitrate:200000,framerate:10,latencyMode:'realtime'});
  const vf=new VideoFrame(canvas,{timestamp:0});ve.encode(vf,{keyFrame:true});vf.close();await ve.flush();ve.close();
  const ae=new AudioEncoder({output:(chunk,metadata)=>{if(!audio)audio=chunk;if(metadata.decoderConfig)audioConfig=metadata.decoderConfig},error:e=>encodeError=e});
  ae.configure({codec:'opus',sampleRate:48000,numberOfChannels:1,bitrate:32000});
  const samples=new Float32Array(960);for(let i=0;i<samples.length;i++)samples[i]=Math.sin(2*Math.PI*440*i/48000)*0.1;
  const ad=new AudioData({format:'f32',sampleRate:48000,numberOfFrames:960,numberOfChannels:1,timestamp:0,data:samples});ae.encode(ad);ad.close();await ae.flush();ae.close();
  if(encodeError||!video||!audio)throw Error('browser encoding failed');
  const description=config=>config?.description?btoa(String.fromCharCode(...new Uint8Array(config.description))):undefined;
  pub.media.send({type:'configure',tracks:[{purpose:'audio',codec:'opus',sample_rate:48000,channels:1,description:description(audioConfig)},{purpose:'camera',codec:'vp8',width:64,height:64,description:description(videoConfig)}]});
  const own=await pub.media.wait(x=>x.type==='tracks'&&x.participant_id===pub.id);
  await sub.media.wait(x=>x.type==='tracks'&&x.participant_id===pub.id);
  const revision=own.tracks[0].revision;
  for(const [purpose,chunk] of [[0,audio],[1,video]]) {
    const data=new Uint8Array(24+chunk.byteLength),view=new DataView(data.buffer);
    data[0]=1;data[1]=purpose;data[2]=chunk.type==='key'?1:0;view.setUint32(4,revision);view.setBigUint64(8,0n);view.setUint32(16,chunk.duration||0);view.setUint32(20,0);chunk.copyTo(data.subarray(24));pub.media.ws.send(data);
  }
  let videoFrames=0,audioFrames=0;
  for(let i=0;i<2;i++) {
    const buffer=await sub.media.wait(x=>x instanceof ArrayBuffer),data=new Uint8Array(buffer),view=new DataView(buffer);
    const header=16,purpose=data[header+1],chunk={type:data[header+2]?'key':'delta',timestamp:Number(view.getBigUint64(header+8)),data:data.subarray(40)};
    if(purpose===1) {
      const decoder=new VideoDecoder({output:frame=>{if(frame.codedWidth!==64||frame.codedHeight!==64)throw Error('video dimensions changed');videoFrames++;frame.close()},error:e=>encodeError=e});
      decoder.configure(videoConfig||{codec:'vp8',codedWidth:64,codedHeight:64});decoder.decode(new EncodedVideoChunk(chunk));await decoder.flush();decoder.close();
    } else {
      const decoder=new AudioDecoder({output:frame=>{audioFrames+=frame.numberOfFrames;frame.close()},error:e=>encodeError=e});
      decoder.configure(audioConfig||{codec:'opus',sampleRate:48000,numberOfChannels:1});decoder.decode(new EncodedAudioChunk(chunk));await decoder.flush();decoder.close();
    }
  }
  if(encodeError||videoFrames!==1||audioFrames===0)throw Error('browser media decode failed');
  return {videoFrames,audioFrames};
} finally { for(const socket of sockets)socket.close(); }
})()`
	result := evaluateBrowser(t, browser, f.server.URL+"/healthz", script)
	var decoded struct {
		VideoFrames int `json:"videoFrames"`
		AudioFrames int `json:"audioFrames"`
	}
	if json.Unmarshal(result, &decoded) != nil || decoded.VideoFrames != 1 || decoded.AudioFrames == 0 {
		t.Fatal("browser media result invalid")
	}
	t.Logf("Chrome encoded, relayed and decoded VP8 (%d frame) and Opus (%d audio samples)", decoded.VideoFrames, decoded.AudioFrames)
}

func evaluateBrowser(t *testing.T, browser, page, script string) json.RawMessage {
	t.Helper()
	scratch := t.TempDir()
	command := exec.Command(browser, "--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", "--disable-extensions", "--disable-background-networking", "--disable-breakpad", "--no-first-run", "--remote-debugging-port=0", "--user-data-dir="+scratch, "about:blank")
	// A new process group and private profile identify exactly this test's
	// browser. Never search for Chrome processes or touch existing containers.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { command.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-done
		}
	})
	var port string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(scratch, "DevToolsActivePort"))
		if err == nil {
			port = strings.Split(string(data), "\n")[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if port == "" {
		t.Fatal("browser debugging endpoint unavailable")
	}
	request, _ := http.NewRequest("PUT", "http://127.0.0.1:"+port+"/json/new?about:blank", nil)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var target struct {
		URL string `json:"webSocketDebuggerUrl"`
	}
	err = json.NewDecoder(response.Body).Decode(&target)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, target.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	id := 0
	call := func(method string, params any) json.RawMessage {
		id++
		data, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var message struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(data, &message) != nil {
				t.Fatal("invalid browser response")
			}
			if message.ID == id {
				if message.Error != nil {
					t.Fatalf("browser command %s failed", method)
				}
				return message.Result
			}
		}
	}
	call("Page.navigate", map[string]string{"url": page})
	// Navigation returns once the new document is committed. Evaluate in its
	// secure localhost origin and await the complete asynchronous codec test.
	result := call("Runtime.evaluate", map[string]any{"expression": script, "awaitPromise": true, "returnByValue": true, "timeout": 30000})
	var value struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if json.Unmarshal(result, &value) != nil || value.Exception != nil {
		t.Fatal(fmt.Sprintf("browser media harness failed: %s", value.Exception))
	}
	return value.Result.Value
}
