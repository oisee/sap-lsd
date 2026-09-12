package main

// composerHTML is the live timeline composer served by serveComposer. It is a
// self-contained page (no external assets): a scene palette with rendered
// thumbnails, a timeline of steps with per-step seconds and speed, and it saves
// the compose-file back to the server, where the next connection picks it up.
const composerHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ODGP Show Composer</title>
<style>
  :root{
    --bg:#0a0e14; --panel:#131a24; --panel2:#0f151d; --line:#22303f;
    --ink:#cdd6e0; --muted:#6a7787; --dim:#495663; --mint:#5ef2b0; --mint-dim:#2f7a5c;
    --amber:#ffb454; --danger:#ff6b6b;
    --mono:"JetBrains Mono",ui-monospace,Menlo,Consolas,monospace;
  }
  *{box-sizing:border-box}
  body{background:var(--bg);color:var(--ink);font-family:var(--mono);margin:0;
    padding:18px;line-height:1.5;font-size:14px}
  h1{font-size:18px;margin:0;letter-spacing:.02em}
  h2{font-size:11px;letter-spacing:.18em;text-transform:uppercase;color:var(--muted);margin:0 0 10px}
  header{display:flex;flex-wrap:wrap;gap:14px 24px;align-items:center;justify-content:space-between;margin-bottom:18px}
  .stat b{color:var(--mint);font-size:22px;font-variant-numeric:tabular-nums}
  .stat small{color:var(--dim);text-transform:uppercase;font-size:10px;letter-spacing:.1em;margin-left:6px}
  .card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px;margin-bottom:16px}
  .pill{font-size:11px;color:var(--muted)}
  #status{font-size:12px;color:var(--mint)}
  #status.saving{color:var(--amber)} #status.err{color:var(--danger)}

  .palette{display:grid;grid-template-columns:repeat(auto-fill,minmax(210px,1fr));gap:10px}
  .chip{border:1px solid var(--line);background:var(--panel2);border-radius:8px;overflow:hidden;
    cursor:pointer;transition:border-color .12s,transform .06s;display:flex;flex-direction:column}
  .chip:hover{border-color:var(--mint-dim);transform:translateY(-1px)}
  .thumbbox{height:70px;overflow:hidden;background:#05080c;border-bottom:1px solid var(--line);position:relative}
  pre.thumb{margin:0;font:3.6px/3.6px var(--mono);color:var(--mint);white-space:pre;padding:2px 3px;
    letter-spacing:0;position:absolute;top:0;left:0}
  .chip .cap{display:flex;justify-content:space-between;align-items:center;padding:6px 9px}
  .chip .cap b{font-weight:500;font-size:13px}
  .chip .cap span{color:var(--dim);font-weight:700}

  .tl{display:flex;flex-direction:column;gap:7px}
  .empty{color:var(--muted);text-align:center;padding:22px}
  .step{display:grid;grid-template-columns:24px 96px 1fr 70px 66px 108px;gap:9px;align-items:center;
    background:var(--panel2);border:1px solid var(--line);border-left:3px solid var(--mint);border-radius:8px;padding:6px 9px}
  .step.cont{border-top-color:transparent;margin-top:-5px;border-top-left-radius:0;border-top-right-radius:0}
  .step .ix{color:var(--dim);text-align:right;font-size:12px}
  .step .nm{color:var(--mint);font-size:13px;overflow:hidden;text-overflow:ellipsis}
  .step.cont .ix{color:var(--mint-dim)}
  .cont-tag{font-size:10px;color:var(--mint);margin-left:6px;opacity:.9}
  .sthumb{height:34px;overflow:hidden;background:#05080c;border-radius:4px}
  .sthumb pre{margin:0;font:3px/3px var(--mono);color:var(--mint);white-space:pre;padding:1px 2px}
  .fld{display:flex;flex-direction:column;gap:1px}
  .fld label{font-size:9px;letter-spacing:.08em;text-transform:uppercase;color:var(--dim)}
  input[type=number]{width:100%;font-family:var(--mono);font-size:13px;background:#0b111a;color:var(--ink);
    border:1px solid var(--line);border-radius:5px;padding:4px 6px;font-variant-numeric:tabular-nums}
  .ctl{display:flex;gap:4px;justify-content:flex-end}
  .ib{background:none;border:1px solid var(--line);color:var(--muted);border-radius:5px;width:24px;height:24px;
    cursor:pointer;font-size:12px;line-height:1;display:grid;place-items:center;font-family:var(--mono)}
  .ib:hover{color:var(--ink);border-color:var(--dim)} .ib.del:hover{color:var(--danger);border-color:var(--danger)}
  .ib:disabled{opacity:.3;cursor:default}
  .btn{font-family:var(--mono);font-size:12px;border:1px solid var(--line);background:var(--panel2);color:var(--ink);
    padding:7px 13px;border-radius:7px;cursor:pointer}
  .btn:hover{border-color:var(--mint-dim)}
  .btn.primary{background:#12271d;border-color:var(--mint-dim);color:var(--mint)}
  .row{display:flex;gap:12px;align-items:center;flex-wrap:wrap}
  .foot{color:var(--dim);font-size:11px;margin-top:14px}
</style></head>
<body>
  <header>
    <div>
      <h1>ODGP Show Composer <span class="pill">live</span></h1>
      <div class="foot" style="margin-top:4px">edits save to the server &mdash; the next SAP GUI to connect plays them</div>
    </div>
    <div class="row">
      <div class="stat"><b id="total">0s</b><small>loop</small> &nbsp; <b id="count">0</b><small>scenes</small></div>
      <label class="fld" style="flex-direction:row;gap:6px;align-items:center"><span style="color:var(--muted);font-size:11px">default s</span>
        <input id="sceneMs" type="number" min="0.5" step="0.5" value="5" style="width:70px"></label>
      <button class="btn primary" id="save">Save</button>
      <span id="status">loading&hellip;</span>
    </div>
  </header>

  <div class="card"><h2>Scenes &mdash; click to add</h2><div class="palette" id="palette"></div></div>
  <div class="card"><h2>Timeline</h2><div class="tl" id="tl"></div></div>

<script>
var THUMBS = {};        // scene name -> [frame strings]
var state = { sceneMs:5, steps:[] };
var frameIx = 0;
var $ = function(s){ return document.querySelector(s); };

function api(m,u,b){ return fetch(u,{method:m,headers:{'Content-Type':'application/json'},body:b?JSON.stringify(b):undefined}); }

function boot(){
  Promise.all([ api('GET','/api/thumbs').then(function(r){return r.json();}),
                api('GET','/api/show').then(function(r){return r.json();}) ])
  .then(function(res){
    var thumbs=res[0], show=res[1];
    thumbs.forEach(function(t){ THUMBS[t.name]=t.frames||[]; });
    palette(thumbs);
    if(show && show.sceneMs) state.sceneMs = (+show.sceneMs)/1000 || 5;
    $('#sceneMs').value = state.sceneMs;
    state.steps = (show && show.show ? show.show : []).map(function(s){
      return { scene:String(s.scene||''), seconds:+s.seconds||0, speed:(+s.speed>0?+s.speed:1) };
    }).filter(function(s){return s.scene;});
    render(); status('ready','');
    setInterval(cycle, 550); // animate the thumbnails
  }).catch(function(e){ status('load failed: '+e,'err'); });
}

function palette(thumbs){
  var p=$('#palette'); p.innerHTML='';
  thumbs.forEach(function(t){
    var c=document.createElement('div'); c.className='chip';
    var box=document.createElement('div'); box.className='thumbbox';
    var pre=document.createElement('pre'); pre.className='thumb'; pre.dataset.scene=t.name;
    pre.textContent=(t.frames&&t.frames[0])||'';
    box.appendChild(pre);
    var cap=document.createElement('div'); cap.className='cap';
    cap.innerHTML='<b>'+t.name+'</b><span>+</span>';
    c.appendChild(box); c.appendChild(cap);
    c.onclick=function(){ state.steps.push({scene:t.name,seconds:0,speed:1}); render(); save(); };
    p.appendChild(c);
  });
}

function cycle(){
  frameIx++;
  document.querySelectorAll('pre.thumb, pre.sthumb-pre').forEach(function(pre){
    var f=THUMBS[pre.dataset.scene]; if(f&&f.length) pre.textContent=f[frameIx%f.length];
  });
}

function secs(st){ return st.seconds>0?st.seconds:state.sceneMs; }

function render(){
  var tl=$('#tl'); tl.innerHTML='';
  if(!state.steps.length) tl.innerHTML='<div class="empty">empty &mdash; click a scene above to add it</div>';
  state.steps.forEach(function(st,i){
    var cont = i>0 && state.steps[i-1].scene===st.scene;
    var el=document.createElement('div'); el.className='step'+(cont?' cont':'');
    var thumb=(THUMBS[st.scene]&&THUMBS[st.scene][0])||'';
    el.innerHTML =
      '<div class="ix">'+(cont?'&#8627;':(i+1<10?'0':'')+(i+1))+'</div>'+
      '<div class="nm">'+st.scene+(cont?'<span class="cont-tag">&#10552;</span>':'')+'</div>'+
      '<div class="sthumb"><pre class="sthumb-pre" data-scene="'+st.scene+'">'+esc(thumb)+'</pre></div>'+
      '<div class="fld"><label>seconds</label><input type="number" min="0" step="0.5" value="'+(st.seconds||'')+'" placeholder="'+state.sceneMs+'" data-i="'+i+'" data-f="seconds"></div>'+
      '<div class="fld"><label>speed &times;</label><input type="number" min="0.1" step="0.1" value="'+st.speed+'" data-i="'+i+'" data-f="speed"></div>'+
      '<div class="ctl">'+
        '<button class="ib" data-dup="'+i+'" title="duplicate">&#9099;</button>'+
        '<button class="ib" data-mv="-1" data-i="'+i+'" '+(i===0?'disabled':'')+'>&uarr;</button>'+
        '<button class="ib" data-mv="1" data-i="'+i+'" '+(i===state.steps.length-1?'disabled':'')+'>&darr;</button>'+
        '<button class="ib del" data-rm="'+i+'">&times;</button>'+
      '</div>';
    tl.appendChild(el);
  });
  var total=state.steps.reduce(function(a,s){return a+secs(s);},0);
  $('#total').textContent=(Math.round(total*10)/10)+'s';
  $('#count').textContent=state.steps.length;
}
function esc(s){ return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }

function toJSON(){
  return { sceneMs: Math.round(state.sceneMs*1000),
    show: state.steps.map(function(s){ return {scene:s.scene, seconds:s.seconds||0, speed:s.speed||1}; }) };
}
var saveT;
function save(){
  clearTimeout(saveT);
  status('saving&hellip;','saving');
  saveT=setTimeout(function(){
    api('POST','/api/show',toJSON()).then(function(r){
      if(!r.ok) throw new Error('HTTP '+r.status);
      status('saved &middot; next connection plays it','');
    }).catch(function(e){ status('save failed: '+e,'err'); });
  }, 350);
}
function status(t,c){ var s=$('#status'); s.innerHTML=t; s.className=c; }

document.addEventListener('input',function(e){
  var t=e.target;
  if(t.id==='sceneMs'){ state.sceneMs=parseFloat(t.value)||5; render(); save(); return; }
  if(t.dataset.f!==undefined){ var i=+t.dataset.i;
    state.steps[i][t.dataset.f]= t.dataset.f==='seconds' ? (parseFloat(t.value)||0) : (parseFloat(t.value)||1);
    render(); save(); }
});
document.addEventListener('click',function(e){
  var t=e.target.closest('button'); if(!t) return;
  if(t.dataset.rm!==undefined){ state.steps.splice(+t.dataset.rm,1); render(); save(); }
  else if(t.dataset.dup!==undefined){ var i=+t.dataset.dup; state.steps.splice(i+1,0,Object.assign({},state.steps[i])); render(); save(); }
  else if(t.dataset.mv!==undefined){ var i=+t.dataset.i, j=i+ +t.dataset.mv;
    if(j>=0&&j<state.steps.length){ var tmp=state.steps[i]; state.steps[i]=state.steps[j]; state.steps[j]=tmp; render(); save(); } }
});
$('#save').onclick=save;
boot();
</script>
</body></html>`
