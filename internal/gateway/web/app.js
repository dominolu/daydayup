(() => {
  'use strict';
  const $ = (s, root = document) => root.querySelector(s);
  const icon = name => `<svg aria-hidden="true"><use href="#i-${name}"/></svg>`;
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const number = n => new Intl.NumberFormat('zh-CN').format(n || 0);
  const compact = n => n >= 1e6 ? `${(n/1e6).toFixed(2)}M` : n >= 1e4 ? `${(n/1e3).toFixed(1)}K` : number(n);
  const time = t => !t || new Date(t).getFullYear() < 2000 ? '尚未检查' : new Date(t).toLocaleTimeString('zh-CN', {hour12:false});
  const dateTime = t => !t || new Date(t).getFullYear() < 2000 ? '—' : new Date(t).toLocaleString('zh-CN', {hour12:false});
  const future = t => t && new Date(t) > new Date();
  let accounts = [], info = {}, page = 'overview', filter = 'all', search = '', providerFilter = '', connected = false;
  let editing = null, selected = null, drawerTab = 'overview', deleteID = null, quotaID = null, polling = false;
  let groups = [], groupFilter = '', defaultGroup = 'default', groupStats = [], modelDetail = '', routingGroupFilter = '', connectGroup = '', middlewareUI = {model_map:{}, model_rename:{}, routing:'', loaded:false};
  let clientKey = '', clientKeyBusy = false, clientKeyEpoch = 0;
  // WorkBuddy browser OAuth login: only the state string is held client-side;
  // uid/tokens are extracted and persisted by the gateway on poll completion.
  let wbOAuth = {state:'', timer:null, active:false};
  const checking = new Set();
  const titles = {overview:['概览','OVERVIEW','你的模型网关，运行状态一目了然。'],accounts:['账号池','ACCOUNT POOL','在一个入口，管理每一份模型算力。'],models:['模型目录','MODEL DIRECTORY','发现可用能力，为每次请求选择合适的模型。'],agents:['Agent 接入','AGENT WIRING','让本机 Agent 直接使用账号池：选模型、写配置、一键还原。'],routing:['路由决策','ROUTING','每次请求选了谁、跳过谁、为什么，一目了然。'],groups:['路由分组','GROUPS','分组是聚合池的边界：每组账号、聚合模型与独立的 Base URL。'],usage:['用量统计','USAGE INSIGHTS','让每个账号的调用与消耗清晰可见。'],sessions:['会话管理','SESSIONS','按会话追踪每次调用、请求内容与 Token 消耗。'],connect:['接口接入','API CONNECTION','一个 Base URL，接入你常用的工具与工作流。'],redaction:['脱敏','REDACTION','敏感内容出网前替换为稳定占位符，返回时自动还原。']};

  // The console has no login screen: the gateway hands this page an HttpOnly
  // same-origin session cookie when it serves /admin/. If the gateway restarts
  // while the page is open the cookie becomes stale, so reload once to obtain
  // a fresh one instead of prompting for a key.
  let reconnecting = false;
  function reconnect() {
    if (reconnecting) return;
    reconnecting = true;
    setTimeout(() => location.reload(), 300);
  }

  async function api(path, options = {}) {
    const response = await fetch('/admin/v1' + path, {method:options.method || 'GET', cache:'no-store', credentials:'same-origin', signal:AbortSignal.timeout(35000), ...(options.body !== undefined ? {headers:{'Content-Type':'application/json'}, body:JSON.stringify(options.body)} : {})});
    const result = await response.json().catch(() => ({}));
    if (!response.ok) {
      if (response.status === 401) {
        // Only a session that used to work is worth reloading for. A first
        // load without any session (for example, cookies blocked) would
        // otherwise reload forever.
        if (connected) reconnect();
        throw new Error(connected ? '管理会话已失效，正在重新连接…' : '无法建立管理会话，请检查浏览器是否允许本站 Cookie。');
      }
      throw new Error(result.error?.message || `请求失败（${response.status}）`);
    }
    return result;
  }
  function toast(message, error = false) {
    const el = document.createElement('div'); el.className = 'toast' + (error ? ' error' : '');
    el.innerHTML = `${icon(error ? 'info' : 'check')}<span>${esc(message)}</span>`;
    $('#toast-region').append(el); setTimeout(() => el.remove(), 5000);
  }
  function status(a) {
    if (!a.enabled) return {key:'disabled', label:'已停用', color:'gray'};
    if (a.credential_status === 'invalid') return {key:'attention',label:'凭证无效',color:'red'};
    if (future(a.cooldown_until)) return {key:'attention',label:'冷却中',color:'amber'};
    if (a.discovery_error) return {key:'attention',label:'检查失败',color:'amber'};
    if (!a.models_checked_at) return {key:'pending',label:'待检查',color:'blue'};
    if (Object.values(a.model_cooldowns || {}).some(future)) return {key:'healthy',label:'部分限流',color:'amber'};
    return {key:'healthy',label:'可用',color:'green'};
  }
  const badge = a => { const s = status(a); return `<span class="badge ${s.color}"><i class="status-dot"></i>${s.label}</span>`; };
  const avatar = a => { const index = [...a.id].reduce((sum,c) => sum+c.charCodeAt(0),0)%4; return `<span class="account-avatar ${['','blue','purple','sand'][index]}">${esc((a.name || a.id).slice(0,1).toUpperCase())}</span>`; };
  function totals() { return accounts.reduce((t,a) => {t.requests += a.stats.requests; t.tokens += a.stats.total_tokens; t.failures += a.stats.failures; t.input += a.stats.input_tokens; t.output += a.stats.output_tokens; t.cached += a.stats.cached_input_tokens; t.unknown += a.stats.requests_without_usage; t.inflight += a.in_flight; return t;}, {requests:0,tokens:0,failures:0,input:0,output:0,cached:0,unknown:0,inflight:0}); }
  function renderMetrics() {
    const t = totals(), healthy = accounts.filter(a => status(a).key === 'healthy').length;
    const items = [
      ['接入账号',accounts.length,'stack',`已接入 ${new Set(accounts.map(a => a.provider)).size} 个平台`,`${accounts.filter(a=>a.enabled).length} 个已启用`],
      ['可用账号',healthy,'check',accounts.length ? `健康占比 ${Math.round(healthy/accounts.length*100)}%` : '等待首次接入',`${accounts.filter(a=>status(a).key==='attention').length} 个需关注`],
      ['累计请求',compact(t.requests),'zap',`进行中 ${t.inflight} 个请求`, t.requests ? `成功率 ${((1-t.failures/t.requests)*100).toFixed(1)}%` : '尚无业务请求'],
      ['Token 总用量',compact(t.tokens),'chart','统计经过本代理的实际用量', t.unknown ? `${t.unknown} 次未返回 usage` : '输入与输出合计']
    ];
    $('#metrics').innerHTML = items.map(([label,value,i,foot,right],index) => `<article class="metric"><div class="metric-label">${label}</div><span class="metric-icon ${index===1?'teal':''}">${icon(i)}</span><div class="metric-value" title="${index===3?number(t.tokens):index===2?number(t.requests):value}">${value}${index===0||index===1?'<small>个</small>':''}</div><div class="metric-foot"><span class="${index===1?'positive':''}">${esc(foot)}</span></div><div class="sub-line">${esc(right)}</div></article>`).join('');
  }
  function empty(title, text, action = '') { return `<div class="empty"><div class="empty-icon">${icon('server')}</div><h3>${esc(title)}</h3><p>${esc(text)}</p>${action}</div>`; }
  function quotaCell(a) {
    if (!a.quota || a.quota_status === 'stale') return `<div class="quota-unknown">${icon(a.quota_status==='stale'?'clock':'info')}${a.quota_status==='stale'?'快照已过期':'尚未接入额度'}</div>`;
    const q = a.quota, b = q.buckets?.find(b=>b.pool==='general') || q.buckets?.[0];
    if (q.remaining_tokens !== null && q.remaining_tokens !== undefined) return `<span class="quota-value">${number(q.remaining_tokens)} <span class="tag">Token</span></span><div class="sub-line">最近一次额度观测</div>`;
    if (!b) return '<span class="quota-unknown">未提供余额</span>';
    return `<div class="quota-value">${number(b.remaining)} <span class="sub-line">${esc(b.unit==='credits'?'积分':b.unit)}</span></div>${b.limit>0?`<progress class="quota-progress" max="${b.limit}" value="${Math.max(0,b.remaining)}"></progress>`:''}<div class="sub-line">${esc(b.pool==='general'?'通用积分':b.pool==='flash-lite'?'Flash-Lite 专属':b.pool)}</div>`;
  }
  function accountsView() {
    const count = key => key==='all' ? accounts.length : accounts.filter(a=>status(a).key===key).length;
    const rows = accounts.filter(a => (filter==='all' || status(a).key===filter) && (!providerFilter || a.provider===providerFilter) && (!groupFilter || (a.group||'default')===groupFilter) && `${a.name} ${a.id} ${a.models.map(m=>m.id).join(' ')}`.toLowerCase().includes(search.toLowerCase()));
    const tabs = [['all','全部账号'],['healthy','可用'],['attention','需关注'],['disabled','已停用']];
    const toolbar = `<div class="table-toolbar"><div class="tabs">${tabs.map(([key,label])=>`<button class="tab ${filter===key?'active':''}" data-action="filter" data-filter="${key}">${label}<span>${count(key)}</span></button>`).join('')}</div><div class="toolbar-filters"><label class="search-box">${icon('search')}<input id="account-search" placeholder="搜索账号、ID 或模型…" value="${esc(search)}" aria-label="搜索账号"></label><select id="provider-filter" aria-label="平台筛选"><option value="">全部平台</option>${platforms.map(p=>`<option value="${esc(p.id)}" ${providerFilter===p.id?'selected':''}>${esc(p.name)}</option>`).join('')}</select><select id="group-filter" aria-label="分组筛选"><option value="">全部组</option>${groups.map(g=>`<option value="${esc(g)}" ${groupFilter===g?'selected':''}>${esc(g)}</option>`).join('')}</select></div></div>`;
    const ver = id => { const v=agentsUI.versions[id]; return v?(v.detected?`<b class="mono">${esc(v.version)}</b>`:`<span class="quota-unknown">${esc((v.error||'未安装').slice(0,20))}</span>`)+(v.update_hint?`<div class="sub-line" title="${esc(v.update_hint)}">${esc(v.update_hint.slice(0,18))}</div>`:'')+'<button class="button text" data-action="agents-version" data-id="'+esc(id)+'">重测</button>':'<span class="sub-line">未探测</span><button class="button text" data-action="agents-version" data-id="'+esc(id)+'">探测</button>'; };
    const table = rows.length ? `<div class="table-scroll"><table><thead><tr><th>账号</th><th>运行状态</th><th>分组</th><th>剩余额度</th><th>累计用量</th><th>最近检查</th><th>操作</th></tr></thead><tbody>${rows.map(a=>`<tr><td><div class="account-cell">${avatar(a)}<div><button class="account-name" data-action="details" data-id="${esc(a.id)}">${esc(a.name||a.id)}</button><div class="account-sub"><span class="provider-mini">${esc(platformName(a.provider))}</span><span class="divider">/</span><span>${esc(a.id)}</span></div></div></div></td><td>${checking.has(a.id)?'<span class="badge blue">检查中…</span>':badge(a)}<div class="sub-line">并发 ${a.in_flight} / ${a.max_in_flight}${Object.keys(a.unavailable_models||{}).length?` · ${Object.keys(a.unavailable_models).length} 个模型无权限`:''}</div></td><td><button class="tag group-tag" data-action="group-jump" data-group="${esc(a.group||'default')}" title="查看该分组">${esc(a.group||'default')}</button>${(a.group||'default')===defaultGroup?'<div class="sub-line">默认分组</div>':''}</td><td>${quotaCell(a)}</td><td><span class="number" title="${number(a.stats.total_tokens)} Token">${compact(a.stats.total_tokens)} <span class="sub-line">Token</span></span><div class="sub-line">${number(a.stats.requests)} 次请求</div></td><td><span class="mono">${time(a.last_checked_at||a.models_checked_at)}</span><div class="sub-line">${a.models.length ? `${a.models.filter(m=>!a.unavailable_models?.[m.id]).length} 个候选模型` : '等待模型发现'}</div></td><td><div class="row-actions"><button class="check-button" data-action="check" data-id="${esc(a.id)}" ${checking.has(a.id)?'disabled':''}>${icon('refresh')}检查</button><button class="icon-button" data-action="edit" data-id="${esc(a.id)}" aria-label="编辑 ${esc(a.name||a.id)}" title="编辑账号">${icon('edit')}</button><button class="icon-button" data-action="details" data-id="${esc(a.id)}" aria-label="查看 ${esc(a.name||a.id)} 详情">${icon('arrow')}</button></div></td></tr>`).join('')}</tbody></table></div>` : empty(accounts.length?'没有匹配的账号':'还没有接入账号',accounts.length?'调整搜索词或状态筛选，再试一次。':'接入第一个平台账号（支持 '+platforms.map(p=>p.name).join(' / ')+'），开始构建你的账号池。',accounts.length?'<button class="button secondary" data-action="clear-filter">清除筛选</button>':'<button class="button primary" data-action="add">'+icon('plus')+'添加第一个账号</button>');
    return `<section class="panel"><div class="panel-header"><div class="panel-title">账号管理 <small>统一管理各平台的额度主体</small></div><span class="live-label">每 10 秒同步</span></div>${toolbar}${table}<div class="table-footer"><span>共 <b>${accounts.length}</b> 个账号${rows.length!==accounts.length?`，显示 ${rows.length} 个`:''}</span><span>调度策略 <b>${middlewareUI.routing==='smart'?'智能（额度窗口优先）':'轮询 · 自动跳过不可用账号'}</b></span></div></section>`;
  }
  // modelCanonical maps a platform's upstream model id to the client-facing
  // name configured in model_rename (per platform, shared by all its accounts).
  function modelCanonical(provider, id) {
    const entries = (middlewareUI.model_rename || {})[provider] || {};
    return entries[id] || id;
  }
  // modelPlatforms lists the platforms that rename one of their ids to canonical.
  function modelOrigins(canonical) {
    const out = [];
    Object.entries(middlewareUI.model_rename || {}).forEach(([provider, entries]) => {
      Object.entries(entries || {}).forEach(([id, renamed]) => { if (renamed === canonical) out.push({provider, model: id}); });
    });
    return out;
  }
  // group === '' aggregates every group; otherwise only that group's accounts.
  function modelsData(group) {
    const map = new Map();
    accounts.filter(a=>!group || groupOf(a)===group).filter(a=>a.enabled && a.credential_status!=='invalid').forEach(a=>a.models.filter(m=>!a.unavailable_models?.[m.id] && (!a.models_allowlist?.length || a.models_allowlist.includes(m.id))).forEach(m=>{
      const id = modelCanonical(a.provider, m.id);
      if (!map.has(id)) map.set(id,{...m, id, accounts:[], renames:[]});
      const entry = map.get(id);
      entry.accounts.push(a);
      if (m.id !== id) entry.renames.push({provider:a.provider, model:m.id});
    }));
    return [...map.values()].sort((a,b)=>a.id.localeCompare(b.id));
  }
  function modelsView() {
    const group = groupFilter || '';
    const models = modelsData(group);
    const groupSelect = `<select id="model-group-filter" aria-label="分组筛选"><option value="">全部分组</option>${groups.map(g=>`<option value="${esc(g)}" ${groupFilter===g?'selected':''}>${esc(g)}</option>`).join('')}</select>`;
    const aliasesOf = m => Object.entries(middlewareUI.model_map||{}).filter(([,u])=>u===m.id).map(([c])=>`<span class="tag">别名 ${esc(c)}</span>`).join('');
    const detailRow = m => {
      if (modelDetail !== m.id) return '';
      const serving = accounts.filter(a => a.enabled && a.credential_status!=='invalid' && (!group || groupOf(a)===group) && a.models.some(x=>modelCanonical(a.provider,x.id)===m.id) && !a.unavailable_models?.[m.id]);
      const scope = group ? esc(group) + ' 组' : '全部分组';
      const accRows = serving.map(a=>`<tr><td><b>${esc(a.name||a.id)}</b><div class="sub-line">${esc(platformName(a.provider))} / ${esc(a.id)}</div></td><td><span class="tag">${esc(groupOf(a))}</span></td><td>${badge(a)}</td><td>${a.model_cooldowns?.[m.id]?'<span class="badge amber">模型冷却中</span>':'<span class="badge green">可调度</span>'}</td></tr>`).join('');
      return `<tr class="model-detail-row"><td colspan="4"><div class="model-detail"><div class="model-detail-head"><b>聚合详情</b><span class="sub-line">${scope}内为 ${esc(m.id)} 提供服务的账号 · 轮询候选</span>${aliasesOf(m)?'<div>'+aliasesOf(m)+'</div>':''}</div>${serving.length?`<div class="table-scroll"><table class="model-serving"><thead><tr><th>账号</th><th>分组</th><th>状态</th><th>模型状态</th></tr></thead><tbody>${accRows}</tbody></table></div>`:'<p class="model-count-note">该范围内当前没有可用账号服务此模型。</p>'}</div></td></tr>`;
    };
    const table = models.length ? `<div class="table-scroll"><table class="model-table"><thead><tr><th>模型</th><th>能力</th><th>上下文窗口</th><th>来源账号</th></tr></thead><tbody>${models.map(m=>`<tr><td><div class="model-cell"><span class="model-icon">${icon(m.output_modalities?.includes('image')?'grid':'cube')}</span><button class="account-name mono" data-action="model-detail" data-id="${esc(m.id)}">${esc(m.id)}</button>${aliasesOf(m)?'<span class="tag">别名</span>':''}${(m.renames||[]).map(r=>`<span class="tag" title="${esc(r.provider)} 的 ${esc(r.model)}">${esc(r.provider)}: ${esc(r.model)}</span>`).join('')}</div></td><td>${[...(m.input_modalities||['text']),...(m.output_modalities?.includes('image')?['image generation']:[])].map(v=>`<span class="tag">${esc(v)}</span>`).join('')}${m.supported_features?.includes('tools')?'<span class="tag">tools</span>':''}</td><td><span class="number">${m.context_length?compact(m.context_length):'—'}</span></td><td><span class="model-count">${m.accounts.length}</span><span class="sub-line"> 个账号已发现</span></td></tr>${detailRow(m)}`).join('')}</tbody></table></div>` : empty('还没有发现模型','添加账号后，点击“检查”或“刷新状态”获取模型目录。');
    const aliasForm = `<div class="alias-form"><input id="alias-client" placeholder="客户端模型名，如 fast" maxlength="64" spellcheck="false"><span class="sub-line">→</span><select id="alias-target"><option value="">选择上游模型…</option>${models.map(m=>`<option value="${esc(m.id)}">${esc(m.id)}</option>`).join('')}</select><button class="button secondary" data-action="alias-add">${icon('plus')}添加别名</button></div>`;
    const aliasRows = Object.entries(middlewareUI.model_map||{}).map(([client,upstream])=>{ const target = models.find(m=>m.id===upstream); return `<div class="alias-row"><b class="mono">${esc(client)}</b><span class="sub-line">→ ${esc(upstream)}${target?` · 组内聚合 ${target.accounts.length} 个账号`:''}</span><button class="button text" data-action="alias-del" data-client="${esc(client)}">${icon('trash')}删除</button></div>`; }).join('');
    const aliasPanel = `<section class="panel" style="margin-top:18px"><div class="panel-header"><div class="panel-title">模型别名 / 聚合名 <small>客户端写一个名字，网关映射到上游真实模型并回写</small></div><span class="tag">MODEL MAP</span></div><div class="panel-body">${aliasRows?aliasRows:'<p class="model-count-note">还没有别名。例如把 "fast" 指到 "deepseek-v4.1-flash"：客户端直接请求模型 fast，网关改写为真实模型，响应的模型名会回写为 fast。</p>'}${aliasForm}</div></section>`;
    return `<section class="panel"><div class="panel-header"><div class="panel-title">模型目录 <small>${models.length} 个候选模型</small></div><div class="toolbar-filters">${groupSelect}<span class="badge green">${esc(groups.length>1?'多分组聚合':'账号池')}</span></div></div>${table}<div class="table-footer">发现目录提供候选能力，具体权限以真实调用结果为准；已确认无权限的模型自动排除。点击模型名查看组内聚合详情。</div></section>${aliasPanel}`;
  }

function usageView() {
    const t = totals(), sorted = [...accounts].sort((a,b)=>b.stats.total_tokens-a.stats.total_tokens), max = Math.max(1,...sorted.map(a=>a.stats.total_tokens));
    return `<div class="split-grid"><section class="panel"><div class="panel-header"><div class="panel-title">账号用量分布</div><span class="live-label">累计 Token</span></div><div class="panel-body">${sorted.length?sorted.map(a=>`<div class="usage-bar"><div class="bar-label"><span>${esc(a.name||a.id)}</span><span class="mono">${number(a.stats.total_tokens)}</span></div><progress max="${max}" value="${a.stats.total_tokens}"></progress></div>`).join(''):'<p class="model-count-note">接入账号后，将在此显示真实累计用量。</p>'}</div></section><section class="panel"><div class="panel-header"><div class="panel-title">用量构成</div><span class="tag">LIFETIME</span></div><div class="panel-body detail-list">${[['输入 Token',number(t.input)],['输出 Token',number(t.output)],['缓存命中输入',number(t.cached)],['已记录总量',number(t.tokens)],['未返回 usage 的请求',number(t.unknown)],['失败尝试',number(t.failures)]].map(([k,v])=>`<div class="detail-row"><span>${k}</span><b class="mono">${v}</b></div>`).join('')}<p class="model-count-note">仅统计经过本代理的调用，不包括其他客户端；缺失 usage 的请求单独记录，不估算为零用量。</p></div></section></div><section class="panel usage-detail"><div class="panel-header"><div class="panel-title">账号明细</div></div><div class="table-scroll"><table><thead><tr><th>账号</th><th>请求</th><th>输入 Token</th><th>输出 Token</th><th>合计</th><th>无 usage</th></tr></thead><tbody>${sorted.map(a=>`<tr><td><button class="account-name" data-action="details" data-id="${esc(a.id)}">${esc(a.name||a.id)}</button></td>${[a.stats.requests,a.stats.input_tokens,a.stats.output_tokens,a.stats.total_tokens,a.stats.requests_without_usage].map(v=>`<td class="mono">${number(v)}</td>`).join('')}</tr>`).join('')}</tbody></table></div></section>`;
  }
  function overviewView() {
    return `<div class="overview-section-title">路由分组 <small>组间隔离 · 专属 Base URL</small></div>${groupsView()}`;
  }
  function shellQuote(value) { return "'" + String(value).replace(/'/g, "'\\''") + "'"; }
  function connectionCode(key = '<GATEWAY_API_KEY>') {
    const candidate = modelsData(connectGroup).find(m=>m.output_modalities?.includes('text')) || modelsData().find(m=>m.output_modalities?.includes('text'));
    const model = candidate?.id || 'sensenova-6.8-flash-lite';
    const body=JSON.stringify({model,messages:[{role:'user',content:'你好'}],stream:true},null,2);
    const prefix = connectGroup && connectGroup !== defaultGroup ? '/groups/' + connectGroup : '';
    const base = location.origin + prefix;
    return `curl ${shellQuote(base+'/v1/chat/completions')} \\\n  -H ${shellQuote('Authorization: Bearer '+key)} \\\n  -H 'X-Session-ID: my-conversation-001' \\\n  -H 'Content-Type: application/json' \\\n  -d ${shellQuote(body)}`;
  }
  function clearClientKey() { clientKey=''; clientKeyBusy=false; clientKeyEpoch++; }
  async function useClientKey(action) {
    if (clientKeyBusy) return;
    const epoch=clientKeyEpoch;
    clientKeyBusy=true; render();
    try {
      const result=await api('/client-key');
      if (!connected || page!=='connect' || epoch!==clientKeyEpoch) return;
      if (typeof result.api_key!=='string' || !result.api_key) throw new Error('未能读取代理访问 Key。');
      await action(result.api_key);
    } finally {
      if (epoch===clientKeyEpoch) { clientKeyBusy=false; if(page==='connect') render(); }
    }
  }
  function groupBaseRows() {
    const list = groups.length ? groups.slice() : ['default'];
    if (!list.includes(defaultGroup)) list.unshift(defaultGroup);
    const rows = list.map(g => {
      const isDef = g === defaultGroup, base = groupBase(g);
      return `<div class="connect-group-row"><span class="tag ${isDef?'teal':''}">${esc(g)}${isDef?'（默认）':''}</span><code>${esc(base)}</code><button class="button text" data-action="copy-text" data-text="${esc(base)}">${icon('copy')}复制</button></div>`;
    }).join('');
    return `<div class="detail-list">${rows}</div>`;
  }
function connectView() {
    const disabled=clientKeyBusy?'disabled':'';
    return `<div class="split-grid"><section class="panel"><div class="panel-header"><div class="panel-title">统一入口</div><button class="button text" data-action="copy-code" ${disabled}>${icon('copy')}复制示例</button></div><div class="panel-body">
      ` + groupBaseRows() + `
      
      <section class="client-credential" aria-labelledby="client-key-title"><div class="credential-heading"><h2 id="client-key-title">代理访问 Key</h2><span class="tag">客户端调用</span></div><code id="client-key-value" class="credential-value" aria-label="代理访问 Key" aria-live="polite">${clientKey?esc(clientKey):'••••••••••••••••••••••••'}</code><div class="credential-actions"><button class="button secondary" data-action="toggle-client-key" aria-pressed="${!!clientKey}" ${disabled}>${icon('key')}${clientKey?'隐藏 Key':'显示 Key'}</button><button class="button secondary" data-action="copy-client-key" ${disabled}>${icon('copy')}复制 Key</button></div><p class="credential-hint">使用此 Key 调用网关。可在启动环境变量 <code>GATEWAY_API_KEY</code> 中设置，修改后重启服务生效。</p></section>
      <p class="connect-note">客户端填写上方 Base URL 与代理访问 Key，网关自动选择上游账号。复制示例会填入当前代理 Key。可请求 <code>GET /v1/models</code> 检查接入。默认分组不带路径前缀；选中分组后示例使用 <code>/groups/组名/v1</code>，也可改用 <code>X-Group</code> 请求头。</p><label class="sr-only" for="connect-group">示例分组</label><select id="connect-group" aria-label="示例分组" style="margin-bottom:10px"><option value="">默认分组</option>${groups.filter(g=>g!==defaultGroup).map(g=>`<option value="${esc(g)}" ${connectGroup===g?'selected':''}>${esc(g)}</option>`).join('')}</select><pre class="code-block">${esc(connectionCode())}</pre></div></section><section class="panel"><div class="panel-header"><div class="panel-title">支持的接口</div><span class="tag">PASSTHROUGH</span></div><div class="panel-body">${[['GET','/v1/models','聚合模型'],['POST','/v1/chat/completions','OpenAI'],['POST','/v1/responses','Responses'],['POST','/v1/messages','Anthropic'],['POST','/v1/images/generations','图片生成'],['POST','/v1/images/edits','JSON 图片编辑']].map(([m,p,d])=>`<div class="endpoint-row"><span class="method">${m}</span><span class="mono">${p}</span><small>${d}</small></div>`).join('')}<p class="connect-note">支持其他 /v1/* HTTP 路径原样透传。复用 <code>X-Session-ID</code> 关联整个会话；未传时从响应头读取网关生成的 ID，再用于后续请求。</p></div></section></div>`;
  }
  // ---- 路由决策 (M3) ----
  let routingUI = {data:null,busy:false,error:''};
  async function loadRouting() {
    routingUI.busy=true; routingUI.error='';
    try { routingUI.data = await api('/routing?limit=120'); }
    catch(error){ routingUI.error=error.message; }
    finally { routingUI.busy=false; if(page==='routing') render(); }
  }
  function hopChain(d) {
    if (!d.hops || !d.hops.length) return '<span class="sub-line">网关本地响应</span>';
    return d.hops.map((h,i)=>`<span class="hop ${h.status>=400?'hop-error':h.retry?'hop-retry':'hop-ok'}">#${i+1} ${esc(h.account||'—')} ${h.status?('HTTP '+h.status):''}${h.reason?' <small>'+esc(h.reason)+'</small>':''}</span>`).join(' → ');
  }
  function routingView() {
    if (routingUI.error) return `<section class="panel"><div class="panel-header"><div class="panel-title">路由决策</div></div><div class="empty"><div class="empty-icon">${icon('route')}</div><h3>读取失败</h3><p>${esc(routingUI.error)}</p><button class="button secondary" data-action="routing-refresh">重试</button></div></section>`;
    const d = routingUI.data;
    if (!d) return `<section class="panel"><div class="empty"><div class="empty-icon">${icon('route')}</div><h3>正在读取路由决策…</h3></div></section>`;
    const cools = d.cooldowns||[];
    const modelCools = Object.entries(d.model_cooldowns||{});
    const waits = [
      ['正在等待请求', d.wait_active||0],
      ['冷却中的账号', cools.length],
      ['模型级限流', modelCools.reduce((n,[,list])=>n+list.length,0)]
    ];
    const metrics = `<div class="metrics" style="grid-template-columns:repeat(3,minmax(0,1fr))">${waits.map(([l,v])=>`<article class="metric"><div class="metric-label">${l}</div><span class="metric-icon">${icon('clock')}</span><div class="metric-value">${v}<small>项</small></div></article>`).join('')}</div>`;
    const coolPanel = `<section class="panel"><div class="panel-header"><div class="panel-title">冷却与等待 <small>限流恢复时间</small></div><span class="live-label">${cools.length||modelCools.length?'实时':'当前无'}</span></div><div class="panel-body">${cools.length?`<div class="detail-list">${cools.map(c=>`<div class="detail-row"><span>账号 <code>${esc(c.account)}</code></span><b>冷却至 ${dateTime(c.until)}</b></div>`).join('')}</div>`:''}${modelCools.length?`<div class="detail-list" style="margin-top:14px">${modelCools.map(([m,list])=>`<div class="detail-row"><span>模型 <code>${esc(m)}</code></span><b>${list.map(x=>esc(x.account)+' '+dateTime(x.until)).join('；')}</b></div>`).join('')}</div>`:''}${!cools.length&&!modelCools.length?'<p class="model-count-note">没有账号处于冷却。429 后网关会自动等待冷却结束并重试，恢复期间每个受限范围每分钟只放行一个请求。</p>':''}</div></section>`;
    const rows = (d.decisions||[]).filter(x=>!routingGroupFilter || (x.group||'default')===routingGroupFilter);
    const emptyRoute = `<div class="empty compact"><div class="empty-icon">${icon('route')}</div><h3>还没有路由记录</h3><p>发出请求后，这里会展示每次请求选了哪个账号、跳过了谁。</p></div>`;
    const table = rows.length?`<div class="table-scroll"><table><thead><tr><th>时间 / 请求</th><th>分组</th><th>模型</th><th>尝试链</th><th>等待</th><th>结果</th></tr></thead><tbody>${rows.map(x=>`<tr><td><b>${dateTime(x.started_at)}</b><div class="sub-line mono">${esc(x.request_id)}</div><div class="sub-line">${esc(x.path)}</div></td><td><b class="mono">${esc(x.model||'—')}</b><div class="sub-line">${esc(x.provider||'')}</div></td><td><button class="tag group-tag" data-action="routing-group" data-group="${esc(x.group||'default')}">${esc(x.group||'default')}</button></td><td>${hopChain(x)}</td><td>${x.pool_waits?(x.pool_wait_ms+' ms · '+x.pool_waits+' 次'):'—'}</td><td><span class="badge ${x.status>=400?'red':x.status>=300?'amber':'green'}">HTTP ${x.status||'—'}</span>${x.failure?`<div class="sub-line">${esc(x.failure)}</div>`:''}</td></tr>`).join('')}</tbody></table></div>`:emptyRoute;
    const groupSel = `<select id="routing-group-filter" aria-label="分组筛选"><option value="">全部分组</option>${groups.map(g=>`<option value="${esc(g)}" ${routingGroupFilter===g?'selected':''}>${esc(g)}</option>`).join('')}</select>`;
    const strategySel = `<select id="routing-strategy" aria-label="全局默认调度策略" title="未单独设置策略的分组使用的默认调度方式"><option value="round_robin" ${middlewareUI.routing!=='smart'?'selected':''}>轮询</option><option value="smart" ${middlewareUI.routing==='smart'?'selected':''}>智能</option></select>`;
    const controls = `<div class="table-toolbar"><div class="tabs"><span class="toolbar-label">分组</span></div><div class="toolbar-filters">${groupSel}<span class="toolbar-label">全局默认</span>${strategySel}<button class="button secondary" data-action="routing-refresh">${icon('refresh')}刷新</button></div></div>`;
    return `${controls}${metrics}<div class="split-grid" style="grid-template-columns:1fr 2fr;align-items:start"><section>${coolPanel}</section><section class="panel"><div class="panel-header"><div class="panel-title">决策时间线 <small>最近 ${rows.length} 条</small></div><button class="button secondary" data-action="routing-refresh">${icon('refresh')}刷新</button></div>${table}</section></div>`;
  }

  // ---- 用量知情页 (M3) ----
  let usageUI = {window:'24h',data:null,busy:false,error:''};
  async function loadUsage() {
    usageUI.busy=true; usageUI.error='';
    try { usageUI.data = await api('/usage?window='+usageUI.window); }
    catch(error){ usageUI.error=error.message; }
    finally { usageUI.busy=false; if(page==='usage') render(); }
  }
  const money = n => n===0?'—':new Intl.NumberFormat('zh-CN',{maximumFractionDigits:4}).format(n);
  function usageCostCell(r) {
    if (r.cost_mode==='price') return `<span class="quota-value">¥${money(r.cost)}</span><div class="sub-line">按配置价格</div>`;
    if (r.cost_mode==='estimated') return `<span class="quota-value">≈${money(r.cost)} 积分</span><div class="sub-line">按积分倍率估算</div>`;
    return `<span class="quota-unknown">未计价</span><div class="sub-line">${r.unpriced?r.unpriced+' 次未计价':''}</div>`;
  }
  function usageTable(title, rows) {
    if (!rows.length) return `<section class="panel"><div class="panel-header"><div class="panel-title">${title}</div></div><div class="empty compact"><div class="empty-icon">${icon('chart')}</div><h3>暂无数据</h3></div></section>`;
    return `<section class="panel"><div class="panel-header"><div class="panel-title">${title} <small>${rows.length} 项</small></div></div><div class="table-scroll"><table><thead><tr><th>${title==='按客户端'?'客户端':'名称'}</th><th>请求</th><th>输入</th><th>输出</th><th>缓存</th><th>合计</th><th>花费（估算）</th></tr></thead><tbody>${rows.map(r=>`<tr><td><b>${esc(r.key||'—')}</b>${r.unpriced?`<div class="sub-line">${r.unpriced} 次未计价</div>`:''}</td><td class="mono">${number(r.requests)}</td><td class="mono">${number(r.input_tokens)}</td><td class="mono">${number(r.output_tokens)}</td><td class="mono">${number(r.cached_input_tokens)}</td><td class="mono">${number(r.total_tokens)}</td><td>${usageCostCell(r)}</td></tr>`).join('')}</tbody></table></div></section>`;
  }
  function usageView() {
    if (usageUI.error) return `<section class="panel"><div class="panel-header"><div class="panel-title">用量统计</div></div><div class="empty"><div class="empty-icon">${icon('chart')}</div><h3>读取失败</h3><p>${esc(usageUI.error)}</p><button class="button secondary" data-action="usage-refresh">重试</button></div></section>`;
    const d = usageUI.data;
    if (!d) return `<section class="panel"><div class="empty"><div class="empty-icon">${icon('chart')}</div><h3>正在读取用量…</h3></div></section>`;
    if (!d.session_storage_enabled) return empty('会话存储未启用','在配置文件中设置 session_store_dir 并重启服务后，才能按客户端与模型统计用量。下方仍显示账号级累计。') + usageTable('按账号（会话关闭，仅累计口径）', d.rows?.accounts||[]);
    const tabs=[['24h','近 24 小时'],['7d','近 7 天'],['all','全部']];
    const summary=d.rows||{};
    const all=summary.clients||[];
    const totals=all.reduce((t,r)=>{t.requests+=r.requests;t.input+=r.input_tokens;t.output+=r.output_tokens;t.cached+=r.cached_input_tokens;t.total+=r.total_tokens;t.cost+=r.cost;return t;},{requests:0,input:0,output:0,cached:0,total:0,cost:0});
    const top=(all[0]||{}).key;
    const metrics=`<div class="metrics"><article class="metric"><div class="metric-label">窗口内请求</div><span class="metric-icon teal">${icon('zap')}</span><div class="metric-value">${number(totals.requests)}<small>次</small></div><div class="metric-foot"><span>${top?'消耗最多的客户端：'+esc(top):'等待请求'}</span></div></article><article class="metric"><div class="metric-label">输入 Token</div><span class="metric-icon">${icon('database')}</span><div class="metric-value">${compact(totals.input)}<small>tok</small></div></article><article class="metric"><div class="metric-label">输出 Token</div><span class="metric-icon">${icon('database')}</span><div class="metric-value">${compact(totals.output)}<small>tok</small></div></article><article class="metric"><div class="metric-label">花费估算</div><span class="metric-icon">${icon('chart')}</span><div class="metric-value">${money(totals.cost)}<small>${d.priced_requests?'已计价':'未计价'}</small></div><div class="metric-foot"><span>${esc(d.cost_estimate_note||'')}</span></div></article></div>`;
    return `<div class="table-toolbar"><div class="tabs">${tabs.map(([k,l])=>`<button class="tab ${usageUI.window===k?'active':''}" data-action="usage-window" data-window="${k}">${l}</button>`).join('')}</div><button class="button secondary" data-action="usage-refresh">${icon('refresh')}刷新</button></div>${metrics}${usageTable('按客户端', summary.clients)}${usageTable('按模型', summary.models)}${usageTable('按账号', summary.accounts)}<section class="panel"><div class="panel-header"><div class="panel-title">计价说明</div></div><div class="panel-body"><p class="model-count-note">${esc(d.cost_estimate_note||'')}</p><p class="model-count-note">已计价 ${number(d.priced_requests||0)} 次 · 估算 ${number(d.estimated_requests||0)} 次 · 未计价 ${number(d.unpriced_requests||0)} 次。在 config.json 的 pricing 段为模型填写每 1M token 的人民币价格；WorkBuddy 模型没有价格时按其官方积分倍率估算。</p></div></section>`;
  }

  let sessionEpoch = 0;
  let sessionsUI = {rows:[],total:0,offset:0,query:'',archived:false,loaded:false,busy:false,detail:null,requestOffset:0,request:null,error:''};
  const sessionPath = id => '/sessions/'+encodeURIComponent(id);
  const usageValue = (usage,key) => usage?.known && (!usage.reported_fields || usage.reported_fields.includes(key)) ? number(usage[key]) : '—';
  function sessionPager(offset,total,action) {
    return `<div class="session-pagination"><span>共 ${number(total)} 条 · 每页 30 条</span><div><button class="button secondary" data-action="${action}" data-offset="${Math.max(0,offset-30)}" ${offset===0?'disabled':''}>上一页</button><button class="button secondary" data-action="${action}" data-offset="${offset+30}" ${offset+30>=total?'disabled':''}>下一页</button></div></div>`;
  }
  function sessionOutcome(row) {
    const label={success:'已完成',error:'请求失败',running:'进行中',interrupted:'已中断'}[row.outcome]||row.outcome;
    return `<span class="badge ${row.outcome==='success'?'green':row.outcome==='running'?'blue':'red'}">${esc(label)}</span>`;
  }
  const knownMetric = value => typeof value==='number' && Number.isFinite(value);
  const secondsMetric = value => knownMetric(value) ? `${new Intl.NumberFormat('zh-CN',{minimumFractionDigits:1,maximumFractionDigits:2}).format(value/1000)} 秒` : '未知';
  const tokenMetric = (usage,key) => usageValue(usage,key)==='—'?'未知':`${usageValue(usage,key)} tok`;
  function insightRow(label,value,description) {
    return `<div class="insight-row" title="${esc(description)}"><span>${esc(label)}</span><b class="${value==='未知'?'insight-unknown':''}">${esc(value)}</b></div>`;
  }
  function sessionUsageCards(s) {
    const p=s.performance||{}, t=s.token_details||{}, requests=typeof s.requests==='number';
    const allCache=(t.cache_samples||0)+(t.cache_unknown_attempts||0);
    const cacheNote=allCache?`缓存统计覆盖 ${t.cache_samples||0} / ${allCache} 次上游尝试。`:'上游尚未提供可核对的缓存明细。';
    const toolNote=p.tool_calls?`工具耗时由客户端上报，累计 ${number(p.tool_calls)} 次。`:'工具在客户端执行，未上报时显示未知。';
    const partial=s.attempts_without_usage || s.requests_with_unknown_usage || s.usage_complete===false&&s.attempts?.length;
    return `<div class="session-insights"><section class="insight-card" aria-label="${requests?'会话':'请求'}统计"><div class="insight-heading"><h3>${icon('clock')}${requests?'会话':'请求'}统计</h3></div>
      ${insightRow('模型用时',secondsMetric(p.model_time_ms),'各次上游尝试的累计耗时，包含网络、重试和响应转发；并发调用的时间会累加。')}
      ${insightRow('工具调用用时',secondsMetric(p.tool_time_ms),toolNote)}
      ${insightRow('首 token 平均（TTFT）',secondsMetric(p.ttft_ms),'上游请求开始至首个有效文本、推理或工具参数增量的平均延迟，排除角色、心跳和 usage。')}
      ${insightRow('输出速度（TPS）',knownMetric(p.tps)?`${new Intl.NumberFormat('zh-CN',{maximumFractionDigits:1}).format(p.tps)} tok/s`:'未知','完整流式响应的输出 Token 数 ÷ 首末有效增量的时间跨度；会话按合计 Token 与合计时长计算。整包到达、缺少用量或流中断时不估算。')}
      <p class="insight-foot">TTFT ${number(p.ttft_samples)} 个样本 · TPS ${number(p.tps_samples)} 个样本<br>${esc(toolNote)}</p>
    </section><section class="insight-card" aria-label="Token 用量"><div class="insight-heading"><h3>${icon('database')}Token 用量</h3><b>${esc(tokenMetric(s.usage,'total_tokens'))}</b></div>
      ${insightRow('缓存命中',knownMetric(t.cache_hit_rate)?`${Math.round(t.cache_hit_rate)}%`:'未知',`缓存读取 Token ÷ 对应输入 Token；${cacheNote}`)}
      ${insightRow('未缓存输入',knownMetric(t.uncached_input_tokens)?`${number(t.uncached_input_tokens)} tok`:'未知','已报告输入 Token − 对应缓存读取 Token；仅累加两项均已报告且一致的尝试。')}
      ${insightRow('缓存读取',tokenMetric(s.usage,'cached_input_tokens'),'上游报告的缓存命中输入 Token；已包含在输入 Token 中，不重复加到总量。')}
      ${insightRow('输出',tokenMetric(s.usage,'output_tokens'),'上游报告的输出 Token，包含上游计入输出的推理或工具参数 Token。')}
      <p class="insight-foot">${esc(cacheNote)}${partial?'已知用量可能不完整。':'缓存读取属于输入的一部分。'}</p>
    </section></div>`;
  }
  function sessionToolEvents(data) {
    const rows=data?.tool_events||[],total=data?.tool_events_total||0;
    if(!total)return '';
    return `<details class="session-tools"><summary>工具调用 · ${number(total)} 次 <span class="tag">客户端上报</span></summary><div class="table-scroll"><table class="session-attempt-table"><thead><tr><th>工具 / 调用 ID</th><th>耗时</th><th>状态</th><th>上报时间</th></tr></thead><tbody>${rows.map(t=>`<tr><td><b>${esc(t.name||'未命名工具')}</b><div class="sub-line mono">${esc(t.id)}</div></td><td>${secondsMetric(t.duration_ms)}</td><td>${esc({success:'已完成',error:'失败',cancelled:'已取消'}[t.status]||t.status)}</td><td>${dateTime(t.reported_at)}</td></tr>`).join('')}</tbody></table></div>${total>rows.length?'<p class="insight-foot">展示最近 30 次上报，可通过管理 API 分页查看全部记录。</p>':''}</details>`;
  }
  function sessionPreview(label,preview,direction,row) {
    if (!preview) return '';
    return `<details class="session-body"><summary>${esc(label)} <span class="sub-line">${preview.truncated?'前 64 KiB 预览':preview.unavailable?'暂不可读':'正文预览'}</span></summary>${preview.incomplete?'<p class="drawer-error">归档不完整或校验失败，请结合请求状态查看。</p>':''}<pre class="code-block">${esc(preview.unavailable?'响应仍在记录或正文暂不可读取。':preview.text)}</pre><button class="button secondary" data-action="session-download" data-direction="${direction}" data-id="${esc(row.id)}" ${preview.unavailable?'disabled':''}>下载完整${label}</button></details>`;
  }
  const identitySource = source => ({'header:x-session-id':'统一会话头','header:x-claude-code-session-id':'Claude Code 原生会话头','header:x-opencode-session':'OpenCode 原生会话头','header:x-deepseek-harness-session-id':'DSH 原生会话头','header:thread-id':'Codex 线程头','header:thread_id':'Codex 线程头','header:session-id':'兼容会话头','header:session_id':'兼容会话头','header:x-session-affinity':'Pi 会话亲和头','body:metadata.session_id':'请求体会话 ID','body:metadata.user_id.session_id':'结构化会话 ID','body:dsh_session_log.session.id':'DSH 会话日志 ID','previous_response_id':'Responses 响应续接',upstream_turn_state:'上游状态续接','tool_continuation':'工具调用链续接',title_context:'标题上下文关联',title_seed:'关联先到的标题',generated:'自动新建',legacy_unknown:'历史来源未知'}[source]||source||'历史来源未知');
  const identityClient = client => client==='Unknown'?'未识别客户端':client;
  const titleDetector = value => ({'dsh:session-title':'DSH 标题模块','opencode:title-agent':'OpenCode 标题代理','claude-code:session-title':'Claude Code 标题模块','compat:dedicated-title-prompt':'专用会话标题提示词','header:x-request-purpose':'请求用途标记','body:metadata.purpose':'请求体用途标记'}[value]||value||'—');
  const titleStatus = value => ({pending:'等待标题返回',applied:'已更新会话名',unassociated:'待关联会话',manual_name_preserved:'保留手动名称',superseded:'已由更新的标题替代',failed:'标题请求失败',invalid_output:'标题输出不合规',storage_error:'名称保存失败'}[value]||value||'—');
  function sessionNaming(s) {
    const n=s.name_info||{},source={id:'使用会话 ID',manual:'手动命名',model_title:'模型自动命名'}[n.source]||'历史名称';
    return `<div class="session-naming"><div><span class="badge ${n.source==='model_title'?'green':n.source==='manual'?'blue':'gray'}">${esc(source)}</span>${n.detector?`<span>${esc(titleDetector(n.detector))}</span>`:''}${n.request_id?`<button class="button text" data-action="session-request" data-id="${esc(n.request_id)}">查看命名请求 ${icon('arrow')}</button>`:''}</div>${s.title_requests?`<p>标题生成 ${number(s.title_requests)} 次 · 已知用量 ${tokenMetric(s.title_usage,'total_tokens')}，已计入会话总用量。</p>`:''}</div>`;
  }
  function titleEvidence(r) {
    const t=r.title;if(!t)return '';
    const association={session_id:'明确会话标识',user_messages:'原始用户消息唯一匹配',unmatched:'尚无匹配的主请求',ambiguous:'多个会话具有相同输入'}[t.association]||t.association;
    return `<section class="session-identity session-title-evidence" aria-label="标题生成识别"><div class="identity-heading"><b>标题生成</b><span class="badge ${t.status==='applied'?'green':t.status==='unassociated'?'amber':'gray'}">${esc(titleStatus(t.status))}</span></div><dl><dt>识别规则</dt><dd>${esc(titleDetector(t.detector))}</dd><dt>关联依据</dt><dd>${esc(association)}</dd>${t.text?`<dt>生成的标题</dt><dd>${esc(t.text)}</dd>`:''}${t.linked_request_id?`<dt>关联主请求</dt><dd><button class="button text" data-action="session-request" data-id="${esc(t.linked_request_id)}">${esc(t.linked_request_id)} ${icon('arrow')}</button></dd>`:''}${t.error?`<dt>诊断</dt><dd><code>${esc(t.error)}</code></dd>`:''}</dl>${t.status==='unassociated'?'<p>本次调用已归档并统计用量；提供明确的会话 ID 可关联标题。存在歧义时不会自动改名。</p>':''}</section>`;
  }
  function identityEvidence(r) {
    const i=r.identity||{},note={no_session_identifier:'未收到会话标识，已新建会话。请为后续请求复用响应头 X-Session-ID。',ambiguous_tool_continuation:'工具调用链对应多个会话，已新建会话。请由客户端提供明确的会话 ID。',unmatched_tool_continuation:'工具调用链未找到完整且唯一的历史记录，已新建会话。'}[i.note];
    return `<section class="session-identity" aria-label="会话识别依据"><div class="identity-heading"><b>会话识别依据</b><span class="badge ${i.source==='generated'?'amber':i.source?'green':'gray'}">${esc(identitySource(i.source))}</span></div><dl><dt>客户端</dt><dd>${esc(identityClient(i.client)||'历史数据未记录')}</dd><dt>识别字段</dt><dd><code>${esc(i.source||'legacy_unknown')}</code></dd>${i.client_session_id?`<dt>客户端会话 ID</dt><dd><code>${esc(i.client_session_id)}</code></dd>`:''}${i.linked_request_id?`<dt>续接上一请求</dt><dd><code>${esc(i.linked_request_id)}</code></dd>`:''}</dl>${note?`<p>${esc(note)}</p>`:''}</section>`;
  }
  function identitySummary(s) {
    return `<div class="session-identity-summary"><div>${(s.clients||[]).map(c=>`<span class="tag">${esc(identityClient(c))}</span>`).join('')} ${(s.identity_sources||['legacy_unknown']).map(src=>`<span class="tag">${esc(identitySource(src))}</span>`).join('')}</div>${s.generated_requests?`<p>${number(s.generated_requests)} 次自动新建，${number(s.linked_requests)} 次通过工具调用链续接。需要完整跨轮归档时，客户端应持续发送稳定会话 ID。</p>`:''}</div>`;
  }
  function sessionAdaptersView() {
    const rows=info.session_adapters||[];
    if(!rows.length)return '';
    return `<details class="panel session-adapters"><summary><span>${icon('code')}智能体会话适配</span><span class="tag">${rows.length} 类接入规则</span></summary><p>优先读取客户端实际发送的标识。缺失时，仅在工具调用 ID 与原请求历史唯一对应时续接；无法确认则新建会话。</p><div class="table-scroll"><table><thead><tr><th>客户端</th><th>识别标识</th><th>说明</th></tr></thead><tbody>${rows.map(r=>`<tr><td><b>${esc(r.client)}</b></td><td>${r.sources.map(src=>`<code>${esc(src)}</code>`).join('<br>')}</td><td>${esc(r.note)}</td></tr>`).join('')}</tbody></table></div><p>识别 DSH、OpenCode、Claude Code 的已知标题请求及专用标题提示词；也可使用 X-Request-Purpose: session-title 明确用途。标题调用会关联到同一会话，用量计入合计，手动名称优先。模型与接口能力仍由上游决定。</p></details>`;
  }
  function sessionRequestView() {
    const detail=sessionsUI.request;
    if(!detail) return '';
    const r=detail.data;
    return `<section class="panel session-exchange"><div class="panel-header"><div><div class="panel-title">请求与响应</div><span class="sub-line mono">${esc(r.id)}</span></div><button class="icon-button" data-action="session-close-request" aria-label="关闭请求明细">${icon('close')}</button></div><div class="panel-body"><div class="session-request-meta"><span class="method">${esc(r.method)}</span><code>${esc(r.path)}</code>${sessionOutcome(r)}<span class="sub-line">HTTP ${r.status||'—'} · ${number(r.duration_ms)} ms · ${esc(r.account_id||'未调用上游')}</span></div>${r.pool_wait_ms?`<p class="session-usage-note">等待账号／限流恢复 ${number(r.pool_wait_ms)} ms · ${number(r.pool_waits)} 次等待</p>`:''}${r.failure?.reason?`<div class="drawer-error">${esc(r.failure.reason)} · ${esc(r.failure.message)}</div>`:''}${r.capture_error?`<div class="drawer-error">${esc(r.capture_error)}</div>`:''}${!r.response.complete?'<p class="session-usage-note">响应尚未完成或已中断，已收到的正文仍保留在归档中。</p>':''}${identityEvidence(r)}${titleEvidence(r)}${sessionUsageCards(r)}${sessionToolEvents(detail)}${sessionPreview('请求',detail.request_preview,'request',r)}${sessionPreview('响应',detail.response_preview,'response',r)}<h3 class="session-subtitle">账号尝试记录</h3>${r.attempts.length?`<div class="table-scroll"><table class="session-attempt-table"><thead><tr><th>次数 / 账号</th><th>响应</th><th>输入 / 输出</th><th>结果</th></tr></thead><tbody>${r.attempts.map(a=>`<tr><td><b>#${a.number}</b> <code>${esc(a.account_id)}</code><div class="sub-line">${number(a.duration_ms)} ms</div></td><td>HTTP ${a.status||'—'}</td><td>${usageValue(a.usage,'input_tokens')} / ${usageValue(a.usage,'output_tokens')}</td><td>${esc(a.failure?.reason||(a.status>=200&&a.status<400?'成功':'失败'))}<div class="sub-line">${a.retry?'等待恢复或切换账号后重试':'最终尝试'}</div></td></tr>`).join('')}</tbody></table></div>`:'<p class="session-usage-note">本次由网关直接响应，没有调用上游账号。</p>'}</div></section>`;
  }
  function sessionsView() {
    const u=sessionsUI;
    if (info.session_storage===false) return empty('会话存储未启用','在配置文件中设置 session_store_dir 并重启服务。');
    const notice=u.error?`<div class="drawer-error" role="alert">${esc(u.error)}</div>`:'';
    if (u.detail) {
      const d=u.detail,s=d.data;
      return `<div class="session-toolbar"><button class="button secondary" data-action="session-back">返回会话列表</button><button class="button secondary" data-action="session-refresh" ${u.busy?'disabled':''}>${icon('refresh')}刷新会话</button></div>${notice}<section class="panel"><div class="panel-header session-heading"><div><div class="panel-title">${esc(s.name)}</div><div class="sub-line mono">${esc(s.id)}</div></div><div class="session-actions"><button class="button secondary" data-action="session-copy">${icon('copy')}复制 ID</button><button class="button secondary" data-action="session-rename">${icon('edit')}重命名</button><button class="button secondary" data-action="session-archive">${s.archived?'恢复会话':'归档会话'}</button></div></div><div class="panel-body"><div class="session-summary"><span>${number(s.requests)} 次请求</span><span>${number(s.failures)} 次失败</span><span>${number(s.in_flight)} 次进行中</span><span>最近活动 ${dateTime(s.updated_at)}</span></div>${identitySummary(s)}${sessionNaming(s)}${sessionUsageCards(s)}${sessionToolEvents(d)}<div class="session-binding">${icon('lock')} 优先固定账号 ${Object.entries(s.preferred_accounts||{}).map(([p,a])=>`<span class="tag">${esc(p)} / ${esc(a)}</span>`).join('')||'<span class="sub-line">尚未调用上游</span>'}</div></div></section><section class="panel session-history"><div class="panel-header"><div class="panel-title">请求时间线</div><span class="tag">${number(d.total)} REQUESTS</span></div>${d.requests.length?`<div class="table-scroll"><table class="session-table"><thead><tr><th>时间 / 请求</th><th>模型 / 账号</th><th>输入 → 输出</th><th>结果</th><th></th></tr></thead><tbody>${d.requests.map(r=>`<tr class="${u.request?.data.id===r.id?'session-row-selected':''}"><td><b>${dateTime(r.started_at)}</b><div class="sub-line mono">${esc(r.method)} ${esc(r.path)}</div><div class="sub-line">${esc(identitySource(r.identity?.source))}${r.purpose==='title'?'<span class="badge blue purpose-badge">标题生成</span>':''}</div></td><td><b>${esc(r.model||'—')}</b><div class="sub-line mono">${esc(r.account_id||'网关直接响应')}</div></td><td>${usageValue(r.usage,'input_tokens')} → ${usageValue(r.usage,'output_tokens')}<div class="sub-line">合计 ${usageValue(r.usage,'total_tokens')}${r.attempts_without_usage?' · 部分未知':''}</div></td><td>${sessionOutcome(r)}<div class="sub-line">HTTP ${r.status||'—'} · ${number(r.duration_ms)} ms</div></td><td><button class="button text" data-action="session-request" data-id="${esc(r.id)}">查看明细</button></td></tr>`).join('')}</tbody></table></div>`:empty('尚无请求','该会话的请求将在此出现。')}${sessionPager(u.requestOffset,d.total,'session-request-page')}</section>${sessionRequestView()}`;
    }
    return `<section class="panel"><div class="panel-header session-heading"><form id="session-search-form" class="session-search"><label class="sr-only" for="session-search">搜索会话</label><input id="session-search" type="search" maxlength="128" placeholder="搜索 ID、名称、客户端或模型" value="${esc(u.query)}"><button class="button secondary" type="submit">${icon('search')}搜索</button></form><div class="session-actions"><button class="button ${u.archived?'primary':'secondary'}" data-action="session-show-archived">${u.archived?'查看活跃会话':'查看已归档'}</button><button class="button secondary" data-action="session-refresh" ${u.busy?'disabled':''}>${icon('refresh')}刷新</button></div></div>${notice}<div class="session-tip">自动识别常用智能体的原生会话标识；也可统一使用 <code>X-Session-ID</code>。响应头返回归档 ID 和识别来源。</div>${!u.loaded?empty('正在读取会话…','加载会话与 Token 用量。'):u.rows.length?`<div class="table-scroll"><table class="session-table"><thead><tr><th>会话</th><th>请求 / 失败</th><th>输入 → 输出</th><th>最近活动</th><th></th></tr></thead><tbody>${u.rows.map(s=>`<tr><td><button class="account-name" data-action="session-open" data-id="${esc(s.id)}">${esc(s.name)}</button><div class="sub-line mono">${esc(s.id)}</div><div class="sub-line">${esc(s.models.join(' · ')||'尚未指定模型')}</div><div class="sub-line">${s.name_info?.source==='model_title'?'<span class="tag">自动标题</span> ':s.title_requests&&s.name_info?.source==='id'?'<span class="tag">标题未采用</span> ':''}${esc((s.clients||[]).map(identityClient).join(' · ')||'历史客户端未知')} · ${esc((s.identity_sources||['legacy_unknown']).map(identitySource).join(' / '))}</div></td><td>${number(s.requests)} / <span class="${s.failures?'negative':''}">${number(s.failures)}</span>${s.in_flight?`<div class="sub-line">${s.in_flight} 次进行中</div>`:''}</td><td>${usageValue(s.usage,'input_tokens')} → ${usageValue(s.usage,'output_tokens')}<div class="sub-line">合计 ${usageValue(s.usage,'total_tokens')}${s.attempts_without_usage?' · 部分未知':''}</div></td><td>${dateTime(s.updated_at)}</td><td><button class="button text" data-action="session-open" data-id="${esc(s.id)}">查看会话 ${icon('arrow')}</button></td></tr>`).join('')}</tbody></table></div>`:empty(u.archived?'暂无归档会话':'还没有会话记录','新请求会自动归档。客户端复用 X-Session-ID，即可累计整个会话的用量。')}${sessionPager(u.offset,u.total,'session-page')}</section>${sessionAdaptersView()}<p class="session-usage-note">请求与最终响应加密保存在本机；保留每次上游尝试的状态和 usage。历史数据从本功能启用后开始记录。</p>`;
  }
  async function loadSessions(id=null, offset=0) {
    const epoch=++sessionEpoch;
    sessionsUI.busy=true;sessionsUI.error='';render();
    try {
      const result=await api(id?`${sessionPath(id)}?offset=${offset}&limit=30`:`/sessions?q=${encodeURIComponent(sessionsUI.query)}&archived=${sessionsUI.archived}&offset=${offset}&limit=30`);
      if(epoch!==sessionEpoch||!connected)return;
      sessionsUI.request=null;
      if(id){sessionsUI.detail=result;sessionsUI.requestOffset=offset;}
      else{sessionsUI.detail=null;sessionsUI.rows=result.data;sessionsUI.total=result.total;sessionsUI.offset=offset;sessionsUI.loaded=true;}
    } catch(error) {if(epoch===sessionEpoch)sessionsUI.error=error.message;}
    finally {if(epoch===sessionEpoch){sessionsUI.busy=false;if(page==='sessions')render();}}
  }
  async function sessionAction(action,element) {
    const u=sessionsUI, id=u.detail?.data.id;
    switch(action) {
      case 'session-open': await loadSessions(element.dataset.id);break;
      case 'session-back': await loadSessions(null,u.offset);break;
      case 'session-refresh': await loadSessions(id||null,id?u.requestOffset:u.offset);break;
      case 'session-show-archived': u.archived=!u.archived;await loadSessions();break;
      case 'session-page': await loadSessions(null,Number(element.dataset.offset));break;
      case 'session-request-page': await loadSessions(id,Number(element.dataset.offset));break;
      case 'session-copy': await navigator.clipboard.writeText(id);toast('会话 ID 已复制');break;
      case 'session-rename': $('#session-name').value=u.detail.data.name;$('#session-edit-error').textContent='';$('#session-edit-dialog').showModal();break;
      case 'session-archive': await api(sessionPath(id),{method:'PUT',body:{archived:!u.detail.data.archived}});toast(u.detail.data.archived?'会话已恢复':'会话已归档，仍可接收同 ID 的后续请求');await loadSessions(id,u.requestOffset);break;
      case 'session-close-request': ++sessionEpoch;u.busy=false;u.request=null;render();break;
      case 'session-request': {
        const epoch=++sessionEpoch;
        const result=await api(`${sessionPath(id)}/requests/${encodeURIComponent(element.dataset.id)}`);
        if(epoch!==sessionEpoch||!connected||page!=='sessions')return;
        u.request=result;render();$('.session-exchange')?.scrollIntoView({behavior:'smooth',block:'start'});break;
      }
      case 'session-download': {
        const epoch=sessionEpoch;
        const response=await fetch('/admin/v1'+sessionPath(id)+'/requests/'+encodeURIComponent(element.dataset.id)+'/'+element.dataset.direction,{credentials:'same-origin',cache:'no-store',signal:AbortSignal.timeout(120000)});
        if(!response.ok){if(response.status===401)reconnect();throw new Error('正文下载失败，请刷新会话状态后重试。');}
        const blob=await response.blob();if(epoch!==sessionEpoch||!connected)return;
        const url=URL.createObjectURL(blob),link=document.createElement('a');link.href=url;link.download=`${element.dataset.id}-${element.dataset.direction}.bin`;link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);break;
      }
    }
  }
  document.addEventListener('submit',async event=>{
    if(event.target.id==='model-rename-form'){
      event.preventDefault();
      if (!modelRename) return;
      const value = $('#model-rename-input').value.trim();
      if (!value) { $('#model-rename-error').textContent = '新模型名不能为空'; return; }
      const button = $('#model-rename-save'); button.disabled = true;
      try {
        await modelRenameSave(modelRename.provider, modelRename.model, value);
        $('#model-rename-dialog').close();
        renderDrawer();
        toast(value === modelRename.model ? '已还原模型名' : `已改名：${modelRename.model} → ${value}`);
      } catch (error) { $('#model-rename-error').textContent = error.message; }
      finally { button.disabled = false; }
    }
    if(event.target.id==='session-search-form'){event.preventDefault();sessionsUI.query=$('#session-search').value.trim();await loadSessions();}
    if(event.target.id==='group-rename-form'){ event.preventDefault(); const name=$('#group-name').value.trim(); if(!name)return; const button=$('#group-rename-save'); button.disabled=true; try{ await api('/admin/v1/groups/'+encodeURIComponent(groupAction.name),{method:'PUT',body:{rename:name}}); $('#group-rename-dialog').close(); await loadAccounts(); await loadGroupStats(); if(page==='overview')render(); toast('分组已重命名：'+groupAction.name+' → '+name); }catch(error){ $('#group-rename-error').textContent=error.message; } finally{ button.disabled=false; } }
      if(event.target.id==='group-delete-form'){ event.preventDefault(); const moveTo=$('#group-move-to').value; const button=$('#group-delete-save'); button.disabled=true; try{ await api('/admin/v1/groups/'+encodeURIComponent(groupAction.name),{method:'DELETE',body:{move_to:moveTo}}); $('#group-delete-dialog').close(); await loadAccounts(); await loadGroupStats(); if(page==='overview')render(); toast('分组已解散，账号移入：'+moveTo); }catch(error){ $('#group-delete-error').textContent=error.message; } finally{ button.disabled=false; } }
      if(event.target.id==='session-edit-form'){
      event.preventDefault();const id=sessionsUI.detail?.data.id;if(!id)return;
      const button=$('#session-save');button.disabled=true;
      try{await api(sessionPath(id),{method:'PUT',body:{name:$('#session-name').value.trim()}});$('#session-edit-dialog').close();await loadSessions(id,sessionsUI.requestOffset);toast('会话名称已保存');}catch(error){$('#session-edit-error').textContent=error.message;}finally{button.disabled=false;}
    }
  });

  let redaction=null, redactionTab='log', redactionSpans=null, redactionKind='';
  async function loadRedaction() { try { redaction=await api('/redaction'); } catch(error) { redaction={error:error.message}; } render(); }
  async function loadRedactionSpans() {
    try { const query=new URLSearchParams(); query.set('limit','300'); if(redactionKind)query.set('kind',redactionKind); redactionSpans=await api('/redaction/spans?'+query.toString()); }
    catch(error){ redactionSpans={error:error.message}; }
    render();
  }
  const redactionShort = v => (v=v||'').length>80 ? v.slice(0,80)+'…' : v;
  function redactionView() {
    if (!redaction) return `<div class="empty-state">正在读取脱敏状态…</div>`;
    if (redaction.error) return `<div class="empty-state"><p>读取失败：${esc(redaction.error)}</p><button class="button secondary" data-action="redaction-refresh">重试</button></div>`;
    if (!redaction.enabled) return `<div class="empty-state"><svg><use href="#i-shield"/></svg><b>脱敏层未启用</b><p>在 config.json 的 <code>redaction</code> 段设置 <code>"enabled": true</code> 后重启网关即可生效。</p></div>`;
    const s=redaction.stats||{};
    const metrics=`<div class="metrics">
      <div class="metric-card"><span>掩码值</span><b>${number(s.masked_values||0)}</b><small>出网前替换为占位符</small></div>
      <div class="metric-card"><span>还原值</span><b>${number(s.restored_values||0)}</b><small>返回客户端时恢复原文</small></div>
      <div class="metric-card"><span>未解析</span><b>${number(s.unresolved_placeholders||0)}</b><small>模型虚构的占位符，原样透传</small></div>
      <div class="metric-card ${s.tool_findings?'warn':''}"><span>危险工具告警</span><b>${number(s.tool_findings||0)}</b><small>observe 模式仅记录</small></div>
      <div class="metric-card"><span>映射条目</span><b>${number(s.mappings||0)}</b><small>稳定占位符 ↔ 原文（加密存储）</small></div>
      <div class="metric-card"><span>脱敏请求</span><b>${number(s.requests||0)}</b><small>已处理的请求数</small></div>
    </div>`;
    const pageTabs=`<div class="drawer-tabs page-tabs">
      <button class="tab ${redactionTab==='log'?'active':''}" data-action="redaction-tab" data-tab="log">脱敏拦截日志</button>
      <button class="tab ${redactionTab==='settings'?'active':''}" data-action="redaction-tab" data-tab="settings">脱敏参数设置</button>
    </div>`;
    return metrics+pageTabs+(redactionTab==='log'?redactionLogBody():redactionSettingsBody(s));
  }
  function redactionLogToolbar() {
    const kindTabs=[['','全部'],['masked','掩码'],['restored','还原']];
    return `<div class="table-toolbar"><div class="tabs">${kindTabs.map(([k,l])=>`<button class="tab ${redactionKind===k?'active':''}" data-action="redaction-kind" data-kind="${k}">${l}</button>`).join('')}</div><div class="toolbar-filters"><button class="button secondary" data-action="redaction-spans-refresh">${icon('refresh')}刷新</button></div></div>`;
  }
  function redactionLogBody() {
    if (!redactionSpans) return `<section class="panel"><div class="empty"><div class="empty-icon">${icon('shield')}</div><h3>正在读取拦截日志…</h3></div></section>`;
    if (redactionSpans.error) return `<section class="panel">${redactionLogToolbar()}<div class="empty"><div class="empty-icon">${icon('shield')}</div><h3>读取失败</h3><p>${esc(redactionSpans.error)}</p><button class="button secondary" data-action="redaction-spans-refresh">重试</button></div></section>`;
    const rows=redactionSpans.data||[];
    if (!rows.length) return `<section class="panel">${redactionLogToolbar()}<div class="empty"><div class="empty-icon">${icon('shield')}</div><h3>暂无脱敏记录</h3><p>客户端请求中出现敏感内容被掩码、或模型回复中占位符被还原时，这里会按时间逐条显示原文 ↔ 占位符映射。</p></div></section>`;
    const body=rows.map(sp=>`<tr><td><span class="mono">${esc(dateTime(sp.time))}</span></td><td>${sp.kind==='restored'?'<span class="badge green">还原</span>':'<span class="badge amber">掩码</span>'}</td><td><span class="tag">${esc(sp.label)}</span></td><td><span class="mono">${esc(sp.placeholder)}</span></td><td><span class="mono" title="${esc(sp.original)}">${esc(redactionShort(sp.original))}</span></td><td><span class="mono">${esc(sp.request_id||'—')}</span></td></tr>`).join('');
    return `<section class="panel">${redactionLogToolbar()}<div class="table-scroll"><table><thead><tr><th>时间</th><th>方向</th><th>规则</th><th>占位符</th><th>原文</th><th>请求 ID</th></tr></thead><tbody>${body}</tbody></table></div><div class="table-footer"><span>显示 <b>${rows.length}</b> 条</span><span>页面保留最近 2000 条映射记录；完整历史见 data/gateway.log 的 <code>redaction span</code> 日志</span></div></section>`;
  }
  function redactionSettingsBody(s) {
    const catalog=Object.entries(redaction.rule_catalog||{});
    const entityRows=Object.entries(s.entities||{}).map(([label,count])=>`<div class="detail-row"><span>${esc(redaction.rule_catalog?.[label]||label)}</span><b>${number(count)}</b></div>`).join('');
    const ruleRows=catalog.map(([label,desc])=>{ const off=(redaction.disabled_rules||[]).includes(label); return `<label class="switch-row"><input type="checkbox" data-action="redaction-toggle-rule" data-label="${esc(label)}" ${off?'':'checked'}><span class="switch"></span><b>${esc(label)}</b><small>${esc(desc)}</small></label>`; }).join('');
    const guard=redaction.tool_guard||{enabled:false,mode:'observe'};
    const customCount=Object.entries(redaction.custom_words||{}).reduce((n,[,ws])=>n+(ws||[]).length,0);
    const params=[['启用状态',redaction.enabled?'已启用':'未启用'],['作用域',redaction.scope||'default'],['映射存储文件',redaction.store_file||'—'],['fail-closed',redaction.fail_closed?'开启（掩码失败拒绝请求）':'关闭（失败时透传明文）'],['最大映射条数',number(redaction.max_mappings||0)],['单值掩码上限',number(redaction.max_token_length||0)+' 字符'],['还原缓冲上限',number((redaction.max_restore_buffer||0)/(1024*1024))+' MB'],['占位符有效期',number((redaction.token_ttl_seconds||0)/86400)+' 天'],['已关闭规则',(redaction.disabled_rules||[]).length?(redaction.disabled_rules||[]).map(esc).join('、'):'全部开启'],['自定义词条数',number(customCount)]];
    return `<div class="panel"><div class="panel-heading"><div><span class="section-overline">CURRENT PARAMETERS</span><h3>脱敏参数设置</h3></div><button class="button secondary" data-action="redaction-refresh">刷新状态</button></div><div class="panel-body detail-list">${params.map(([k,v])=>`<div class="detail-row"><span>${k}</span><b>${v}</b></div>`).join('')}</div></div>
    <div class="panel"><div class="panel-heading"><div><span class="section-overline">ENTITY MAPPING</span><h3>实体命中分布</h3></div></div>${entityRows?`<div class="panel-body detail-list">${entityRows}</div>`:empty('暂无命中','客户端请求中出现邮箱、手机号、密钥等敏感内容后，这里会按类型累计。')}</div>
    <div class="panel"><div class="panel-heading"><div><span class="section-overline">BUILTIN RULES</span><h3>内置规则开关</h3></div></div><div class="panel-body"><p class="model-count-note">关闭某条规则后立即生效（热更新，无需重启）。密钥与 PII 规则默认全部开启。</p><div class="switch-grid">${ruleRows}</div></div></div>
    <div class="panel"><div class="panel-heading"><div><span class="section-overline">TOOL GUARD</span><h3>危险工具调用防护</h3></div></div><div class="panel-body"><label class="switch-row"><input type="checkbox" data-action="redaction-toggle-toolguard" ${guard.enabled?'checked':''}><span class="switch"></span><b>${guard.enabled?'已开启（observe）':'已关闭'}</b><small>检测 rm -rf、base64 管道执行、curl|sh 等危险模式，当前仅记录并计数，不阻断。</small></label></div></div>
    <div class="panel"><div class="panel-heading"><div><span class="section-overline">CUSTOM WORDS</span><h3>自定义敏感词与正则</h3></div><button class="button primary" data-action="redaction-save-words">保存规则</button></div><div class="panel-body"><p class="model-count-note">JSON 格式：{"标签":["字面词","re:正则"]}。保存后立即热更新；错误的正则会拒绝保存。</p><textarea id="redaction-words" rows="6" spellcheck="false">${esc(JSON.stringify(redaction.custom_words||{},null,2))}</textarea><p class="model-count-note">存储：${esc(redaction.store_file||'')} · 作用域：${esc(redaction.scope||'')} · fail-closed：${redaction.fail_closed?'开启':'关闭'}</p></div></div>`;
  }
  
  // ---- 路由分组 (aggregate-pool boundaries) ----
  let groupAction = null;
  async function loadGroupStats() {
    try { const r = await api('/groups'); defaultGroup = r.config_default_group || defaultGroup; groupStats = r.data || []; }
    catch (error) { groupStats = []; }
  }
  async function loadMiddleware() {
    try { const r = await api('/middleware'); middlewareUI = {model_map: r.model_map || {}, model_rename: r.model_rename || {}, routing: r.routing || '', loaded: true}; }
    catch (error) { middlewareUI = {model_map: {}, model_rename: {}, routing: '', loaded: false, error: error.message}; }
  }
  const groupOf = a => (a && a.group) || 'default';
  function groupStat(name) { return groupStats.find(g => g.name === name) || {name, accounts: 0, enabled: 0, models: 0, requests: 0, failures: 0}; }
  function groupBase(g) { return (g === defaultGroup) ? location.origin + '/v1' : location.origin + '/groups/' + g + '/v1'; }
  function groupsView() {
    if (!groups.length) return `<section class="panel"><div class="panel-header"><div class="panel-title">路由分组</div><span class="badge gray">尚未创建</span></div><div class="group-hint">${icon('info')}<div><b>分组随账号出现</b>　为账号的「所属分组」填一个名字即创建该组。每组拥有独立的 Base URL（<code>/groups/组名/v1</code>），客户端访问它就固定在这组账号内调度，组间完全隔离。</div></div></section>`;
    const fallbackModels = {};
    accounts.forEach(a => { const g = groupOf(a); (fallbackModels[g] = fallbackModels[g] || new Set()); a.models.forEach(m => fallbackModels[g].add(m.id)); });
    const cards = groups.map(g => {
      const s = groupStat(g), isDefault = g === defaultGroup, base = groupBase(g);
      const models = Number.isFinite(s.models) && s.models > 0 ? s.models : (fallbackModels[g] ? fallbackModels[g].size : 0); const smart = (s.routing_override || middlewareUI.routing || 'round_robin') === 'smart'; const smartTip = '智能（smart）：优先选择额度窗口最早重置、剩余额度最少的账号，减少窗口重置时的额度浪费；需要账号带有额度窗口信息。'; const roundTip = '轮询（round_robin）：组内同模型多账号按顺序均匀分发，自动跳过不可用账号，无额外开销。'; const tip = smart ? smartTip : roundTip;
      return `<article class="group-card"><div class="group-card-head"><b class="mono">${esc(g)}</b>${isDefault?'<span class="tag teal">默认分组</span>':''}</div><div class="group-card-stats"><span>${number(s.accounts)} 个账号</span><span>${number(s.enabled)} 启用</span><span>${number(models)} 个聚合模型</span><span>${number(s.requests)} 请求 · 失败 ${number(s.failures)}</span></div><div class="group-card-base">${icon('code')}<code>${esc(base)}</code><button class="button text" data-action="copy-text" data-text="${esc(base)}">${icon('copy')}复制</button></div><div class="group-card-strategy"><span class="group-card-strategy-label" title="${tip}">调度策略<svg class="tip-dot" aria-hidden="true"><use href="#i-info"/></svg></span><label class="strategy-switch" title="${tip}"><input type="checkbox" data-action="group-strategy" data-group="${esc(g)}" ${smart?'checked':''} aria-label="分组 ${esc(g)} 调度策略"><span class="switch"></span><b>${smart?'智能':'轮询'}</b></label></div><div class="group-card-actions">${isDefault?'':'<button class="button text" data-action="group-default" data-group="'+esc(g)+'">设为默认</button>'}<button class="button text" data-action="group-rename" data-group="${esc(g)}">${icon('edit')}重命名</button><button class="button text" data-action="group-delete" data-group="${esc(g)}">${icon('trash')}解散</button><button class="button text" data-action="group-accounts" data-group="${esc(g)}">查看账号</button></div></article>`;
    }).join('');
    return `<div class="group-hint">${icon('info')}<div><b>分组随账号出现</b>　新建分组只需在账号表单的「所属分组」中填新名字；解散分组会把组内账号移到所选分组。每个分组对应一个可直接复制的 Base URL。<button class="button secondary" data-action="add">${icon('plus')}添加账号并归组</button></div></div><div class="group-grid">${cards}</div>`;
  }

function render() {
    const [title,english,subtitle] = titles[page];
    $('#breadcrumb-page').textContent = title; $('#page-title').innerHTML = `${title} <span>${english}</span>`; $('#page-subtitle').textContent = subtitle;
    document.querySelectorAll('.nav-item').forEach(el=>el.classList.toggle('active',el.dataset.page===page));
    $('#nav-count').textContent = accounts.length;
    // Summary metrics and the add/refresh actions belong to the overview only;
    // every other page shows just its own content.
    const overview = page === 'overview';
    $('#heading-actions').hidden = !overview;
    $('#metrics').hidden = !overview;
    if (overview) renderMetrics(); else $('#metrics').innerHTML = '';
    $('#page-content').innerHTML = ({accounts:accountsView,models:modelsView,usage:usageView,overview:overviewView,connect:connectView,sessions:sessionsView,redaction:redactionView,agents:agentsView,routing:routingView})[page]();
    if (page==='usage') $('#page-content').lastElementChild?.classList.add('usage-detail');
    if (selected) renderDrawer();
  }

  // ---- Agent 接入 (M1) ----
  let agentsUI = {rows:[],profiles:[],mode:'auto',busy:false,error:'',disabled:false,bind:null};
  function profilesView() {
    const save = () => {
      const snapshot = {};
      agentsUI.rows.forEach(a => { if (a.binding?.model) snapshot[a.id] = a.binding.model; });
      return snapshot;
    };
    const chips = agentsUI.profiles.map(p=>`<span class="profile-chip"><b>${esc(p.name)}</b><small>${Object.keys(p.agents||{}).length} 个 Agent</small><button class="button text" data-action="profiles-apply" data-name="${esc(p.name)}" title="一键应用此方案">${icon('zap')}应用</button><button class="icon-button" data-action="profiles-delete" data-name="${esc(p.name)}" title="删除方案" aria-label="删除方案 ${esc(p.name)}">${icon('close')}</button></span>`).join('');
    return `<section class="panel" style="margin-bottom:18px"><div class="panel-header"><div class="panel-title">方案（Profiles）<small>把当前所有 Agent 的模型选择存成一个名字，一键切换</small></div></div><div class="panel-body"><div class="profile-save"><input id="profile-name" type="text" maxlength="40" placeholder="方案名称，如：省钱 / 专注" spellcheck="false"><button class="button secondary" data-action="profiles-save">${icon('plus')}保存当前选择</button></div>${chips?`<div class="profile-chips">${chips}</div>`:'<p class="model-count-note">还没有方案。选择好每个 Agent 的模型后，在这里存成一个名字。</p>'}</div></section>`;
  }
  function agentStatusBadge(row) {
    const map = {unbound:['gray','未接入'],snippet:['blue','片段模式'],ok:['green','已接入'],drift:['amber','配置漂移'],missing:['red','文件缺失'],error:['red','写入失败']};
    const [color,label] = map[row.status] || ['gray',row.status];
    return `<span class="badge ${color}"><i class="status-dot"></i>${label}</span>`;
  }
  function agentModelOptions(selected) {
    const models = modelsData();
    const seen = new Set();
    const opts = [];
    for (const m of models) {
      const key = m.id;
      if (seen.has(key)) continue; seen.add(key);
      const provider = m.provider || m.accounts?.[0]?.provider || 'sensenova';
      opts.push(`<option value="${esc(key)}" ${key===selected?'selected':''}>${esc(provider)}/${esc(key)}</option>`);
    }
    return opts.join('');
  }
  function agentsView() {
    if (agentsUI.disabled) return empty('Agent 接入未启用','在 config.json 的 agents 段设置 "enabled": true 后重启网关。');
    if (agentsUI.error) return `<section class="panel"><div class="panel-header"><div class="panel-title">Agent 接入</div></div><div class="empty"><div class="empty-icon">${icon('agent')}</div><h3>读取失败</h3><p>${esc(agentsUI.error)}</p><button class="button secondary" data-action="agents-refresh">重试</button></div></section>`;
    const rows = agentsUI.rows;
    const modeTabs = [['auto','自动写入'],['snippet','仅生成片段']];
    const ver = id => { const v=agentsUI.versions[id]; return v?(v.detected?`<b class="mono">${esc(v.version)}</b>`:`<span class="quota-unknown">${esc((v.error||'未安装').slice(0,20))}</span>`)+(v.update_hint?`<div class="sub-line" title="${esc(v.update_hint)}">${esc(v.update_hint.slice(0,18))}</div>`:'')+'<button class="button text" data-action="agents-version" data-id="'+esc(id)+'">重测</button>':'<span class="sub-line">未探测</span><button class="button text" data-action="agents-version" data-id="'+esc(id)+'">探测</button>'; };
    const table = rows.length ? `<div class="table-scroll"><table><thead><tr><th>Agent</th><th>接入状态</th><th>绑定模型</th><th>CLI 版本</th><th>自检</th><th>操作</th></tr></thead><tbody>${rows.map(a=>`<tr><td><div class="account-cell"><span class="account-avatar blue">${esc((a.name||a.id).slice(0,1).toUpperCase())}</span><div><b>${esc(a.name||a.id)}</b><div class="account-sub"><span class="provider-mini">${esc(a.id)}</span>${a.requires_bridge?`<span class="divider">/</span><span class="tag">需协议桥 ${esc(a.requires_bridge)}</span>`:''}</div><div class="sub-line">${esc(a.hint||'')}</div></div></div></td><td>${agentStatusBadge(a)}${a.drift_note?`<div class="sub-line">${esc(a.drift_note)}</div>`:''}${a.has_backup?'<div class="sub-line">已有可还原备份</div>':''}</td><td>${a.binding?.model?`<b class="mono">${esc(a.binding.provider?a.binding.provider+'/':'')}${esc(a.binding.model)}</b><div class="sub-line">模式：${a.binding.mode==='snippet'?'仅生成片段':'自动写入'}</div>`:'<span class="quota-unknown">未选择模型</span>'}</td><td>${ver(a.id)}</td><td>${a.binding?.last_check?`${a.binding.check_ok?'<span class="badge green">通过</span>':'<span class="badge red">失败</span>'}<div class="sub-line" title="${esc(a.binding.check_note||'')}">${esc((a.binding.check_note||'').slice(0,40))}</div>`:'<span class="sub-line">未检查</span>'}</td><td><div class="row-actions"><button class="check-button" data-action="agents-bind" data-id="${esc(a.id)}">${icon('edit')}选模型</button>${a.binding?.model?`<button class="check-button" data-action="agents-check" data-id="${esc(a.id)}" ${agentsUI.busy?'disabled':''}>${icon('refresh')}自检</button>`:''}${a.has_backup?`<button class="icon-button" data-action="agents-restore" data-id="${esc(a.id)}" title="还原配置备份">${icon('refresh')}</button>`:''}</div></td></tr>`).join('')}</tbody></table></div>` : empty('还没有可接入的 Agent','本机常用 CLI Agent（DSH、OpenCode、Claude Code、Codex、Cline）都可以通过这里接入网关。');
    return `<section class="panel"><div class="panel-header"><div class="panel-title">Agent 接入 <small>让本机 Agent 直接用上账号池</small></div><div class="toolbar-filters"><div class="tabs">${modeTabs.map(([key,label])=>`<button class="tab ${agentsUI.mode===key?'active':''}" data-action="agents-mode" data-mode="${key}">${label}</button>`).join('')}</div><button class="button secondary" data-action="agents-refresh">${icon('refresh')}刷新</button></div></div>${table}<div class="table-footer"><span>写模式默认 <b>${esc(agentsUI.mode)}</b> · 自动写入会先备份原文件，检测到外部修改时拒绝覆盖</span><span>Claude Code / Codex 需要先开启对应的协议桥（bridges.*），否则接入后请求会返回 400</span></div></section>`;
  }
  async function loadAgents() {
    agentsUI.busy = true; agentsUI.error = '';
    try {
      const result = await api('/agents');
      agentsUI.disabled = !result.enabled;
      agentsUI.mode = result.write_mode || 'auto';
      agentsUI.rows = result.data || [];
      agentsUI.profiles = result.profiles || [];
    } catch (error) { agentsUI.error = error.message; }
    finally { agentsUI.busy = false; if (page==='agents') render(); }
  }
  function openAgentDialog(id) {
    const row = agentsUI.rows.find(a=>a.id===id); if (!row) return;
    agentsUI.bind = row;
    $('#agent-dialog-title').textContent = `接入 ${row.name||row.id}`;
    $('#agent-dialog-intro').textContent = `把“${row.name||row.id}”指到网关。选择模型后${agentsUI.mode==='auto'?'自动写入配置文件':'生成可粘贴的配置片段'}。`;
    $('#agent-model').innerHTML = '<option value="">选择模型…</option>' + agentModelOptions(row.binding?.model);
    $('#agent-model').value = row.binding?.model || '';
    $('#agent-mode').value = row.binding?.mode || agentsUI.mode;
    $('#agent-target').value = row.binding?.target_file || row.target_file || '';
    $('#agent-target-field').hidden = false;
    $('#agent-preview-area').hidden = true;
    $('#agent-error').textContent = '';
    const needs = row.requires_bridge ? {messages:'Claude Code 走 Anthropic 协议：需要开启 bridges.messages 才能接入。', responses:'Codex 走 Responses 协议：需要开启 bridges.responses 才能接入。'}[row.requires_bridge] : '';
    if (needs) { $('#agent-bridge-note').hidden = false; $('#agent-bridge-note-text').textContent = needs; } else { $('#agent-bridge-note').hidden = true; }
    $('#agent-dialog').showModal();
  }
  function agentDialogPayload() {
    return { model: $('#agent-model').value, mode: $('#agent-mode').value, target_file: $('#agent-target').value.trim() || undefined };
  }
  async function agentPreview() {
    const row = agentsUI.bind; if (!row) return;
    $('#agent-error').textContent = '';
    try {
      const result = await api(`/agents/${encodeURIComponent(row.id)}/bind`, {method:'POST', body:{...agentDialogPayload(), preview:true}});
      $('#agent-preview').textContent = result.snippet || result.content || '';
      $('#agent-preview-area').hidden = !(result.snippet || result.content);
    } catch (error) { $('#agent-error').textContent = error.message; }
  }
  function wbTasksSection(a) {
    if (a.provider!=='workbuddy') return '';
    const today = new Date().toISOString().slice(0,10).replace(/-/g,'-');
    const inDate = key => (a[key]||'').slice(0,10) === today.slice(0,10);
    const ci = a.wb_checkin_info||{};
    if (a.realm==='cn') {
      const rows = [
        ['今日签到', ci.today_checked_in===true|| inDate('wb_checkin_date') ? '✅ 已签到' : '⏳ 未签到'],
        ['连续签到', ci.streak_days?`${number(ci.streak_days)} 天`: '—'],
        ['每日签到积分', ci.daily_credit?`${number(ci.daily_credit)} 积分`: '—'],
        ['今日已得', ci.today_credit?`${number(ci.today_credit)} 积分`: '—']
      ];
      return `<div class="drawer-subheading">每日福利（国内版）</div><div class="detail-list">${rows.map(([k,v])=>`<div class="detail-row"><span>${k}</span><b>${esc(v)}</b></div>`).join('')}</div><div class="drawer-actions"><button class="button secondary" data-action="wb-task" data-kind="checkin" data-id="${esc(a.id)}" ${cwTaskBusy? 'disabled':''}>${icon('zap')}每日签到</button><button class="button secondary" data-action="wb-task" data-kind="activity" data-id="${esc(a.id)}">${icon('zap')}活跃上报</button></div>`;
    }
    if (a.realm==='intl') {
      const done = inDate('wb_daily_chat_date');
      return `<div class="drawer-subheading">每日福利（国际版）</div><div class="detail-list"><div class="detail-row"><span>今日活跃打卡</span><b>${done?'✅ 已完成':'⏳ 未完成'}</b></div></div><div class="drawer-actions"><button class="button secondary" data-action="wb-task" data-kind="daily-chat" data-id="${esc(a.id)}" ${cwTaskBusy?'disabled':''}>${icon('zap')}每日活跃打卡</button></div>`;
    }
    return '';
  }
  let cwTaskBusy = false;
  function renderDrawer() {
    const a = accounts.find(a=>a.id===selected); if (!a) { closeDrawer(); return; }
    const stateDetails = [['账号 ID',a.id],['平台',platformName(a.provider)],['Base URL',a.base_url],['凭证来源',a.key_source==='encrypted_store'?'本地加密存储':'环境变量'],['最大并发',`${a.in_flight} / ${a.max_in_flight}`],['最近检查',dateTime(a.last_checked_at||a.models_checked_at)],['最近请求',dateTime(a.stats.last_request)],['最后响应',a.stats.last_status?`HTTP ${a.stats.last_status}`:'—']];
    let body = '';
    if (drawerTab==='overview') body = `<div class="drawer-metrics"><div class="drawer-metric"><span>累计请求</span><b>${number(a.stats.requests)}</b></div><div class="drawer-metric"><span>Token 用量</span><b>${compact(a.stats.total_tokens)}</b></div></div><div class="drawer-subheading">连接信息</div><div class="detail-list">${stateDetails.map(([k,v])=>`<div class="detail-row"><span>${k}</span><b class="${k==='Base URL'||k==='账号 ID'?'mono':''}">${esc(v)}</b></div>`).join('')}</div>${a.discovery_error?`<div class="drawer-error">${icon('info')} ${esc(a.discovery_error)}</div>`:''}${Object.keys(a.model_cooldowns||{}).length?`<div class="drawer-subheading">模型冷却</div>${Object.entries(a.model_cooldowns).map(([m,t])=>`<div class="drawer-model"><b>${esc(m)}</b><span class="sub-line">恢复时间 ${dateTime(t)}</span></div>`).join('')}`:''}${wbTasksSection(a)}<div class="drawer-subheading">账号操作</div><div class="drawer-actions"><button class="button secondary" data-action="toggle" data-id="${esc(a.id)}">${a.enabled?'停用账号':'启用账号'}</button><button class="button text" data-action="delete" data-id="${esc(a.id)}">${icon('trash')}移除账号</button></div>`;
    if (drawerTab==='models') {
      const renames = (middlewareUI.model_rename||{})[a.provider] || {};
      body = `<p class="model-count-note">来源：账号模型发现。权限以真实调用为准，已确认的限流或套餐限制会单独标记。改名按平台生效（${esc(platformName(a.provider))} 的所有账号共用）：客户端与 GET /v1/models 看到新名字，发给平台时自动改回原 id。</p>${a.models.length?a.models.map(m=>{const renamed=renames[m.id];return `<div class="drawer-model"><b>${esc(m.id)}</b>${renamed?`<span class="tag">对外名称 ${esc(renamed)}</span>`:''}${a.unavailable_models?.[m.id]?'<span class="badge red">当前套餐不可用</span>':a.model_cooldowns?.[m.id]?'<span class="badge amber">模型冷却中</span>':'<span class="badge green">已发现</span>'} ${m.context_length?`<span class="tag">${compact(m.context_length)} context</span>`:''}${m.supported_features?.includes('tools')?'<span class="tag">tools</span>':''}<div class="row-actions"><button class="button text" data-action="model-rename" data-id="${esc(a.id)}" data-model="${esc(m.id)}">${icon('edit')}${renamed?'修改改名':'改名'}</button></div></div>`;}).join(''):empty('暂无模型','先检查账号以获取模型目录。')}`;
    }
    if (drawerTab==='quota') body = `<div class="drawer-subheading">额度状态 <span class="badge ${a.quota_status==='reported'?'green':'gray'}">${a.quota_status==='reported'?'已上报':a.quota_status==='stale'?'快照过期':'尚未接入'}</span></div>${a.quota?`<div class="detail-list"><div class="detail-row"><span>来源</span><b>${esc(a.quota.source)}</b></div><div class="detail-row"><span>观测时间</span><b>${dateTime(a.quota.observed_at)}</b></div><div class="detail-row"><span>有效至</span><b>${dateTime(a.quota.valid_until)}</b></div></div><div class="drawer-subheading">积分与 Token</div>${a.quota.remaining_tokens!==null?`<div class="quota-bucket"><div class="bar-label"><span>剩余 Token</span><b>${number(a.quota.remaining_tokens)}</b></div></div>`:''}${(a.quota.buckets||[]).map(b=>`<div class="quota-bucket"><div class="bar-label"><span>${esc(b.name)}</span><b>${number(b.remaining)} ${esc(b.unit)}</b></div>${b.limit>0?`<progress class="quota-progress" value="${b.remaining}" max="${b.limit}"></progress>`:''}</div>`).join('')}`:''}<div class="drawer-actions"><button class="button primary" data-action="quota" data-id="${esc(a.id)}">更新额度快照</button>${a.quota?`<button class="button secondary" data-action="clear-quota" data-id="${esc(a.id)}">清除快照</button>`:''}</div>`;
    $('#account-drawer').innerHTML = `<div class="drawer-header"><div class="drawer-toolbar"><span>ACCOUNT DETAILS</span><button class="icon-button" data-action="close-drawer" aria-label="关闭账号详情">${icon('close')}</button></div><div class="drawer-account">${avatar(a)}<div><h2>${esc(a.name||a.id)}</h2><small>${esc(a.id)}</small></div>${badge(a)}</div><div class="drawer-actions"><button class="button secondary" data-action="check" data-id="${esc(a.id)}" ${checking.has(a.id)?'disabled':''}>${icon('refresh')}${checking.has(a.id)?'检查中…':'检查状态'}</button><button class="button secondary" data-action="edit" data-id="${esc(a.id)}">${icon('edit')}编辑账号</button></div></div><div class="drawer-tabs">${[['overview','概况'],['models','模型目录'],['quota','额度观测']].map(([key,label])=>`<button class="tab ${drawerTab===key?'active':''}" data-action="drawer-tab" data-tab="${key}">${label}</button>`).join('')}</div><div class="drawer-body">${body}</div>`;
  }
  function openDrawer(id) { selected=id; drawerTab='overview'; $('.app-shell').inert=true; $('#drawer-backdrop').hidden=false; $('#account-drawer').hidden=false; document.body.classList.add('modal-open'); renderDrawer(); if(!middlewareUI.loaded){ loadMiddleware().then(()=>{ if(selected===id) renderDrawer(); }); } $('#account-drawer').setAttribute('tabindex','-1'); $('#account-drawer').focus(); }
  let modelRename = null;
  function openModelRename(accountID, model) {
    const account = accounts.find(a => a.id === accountID);
    if (!account) return;
    modelRename = {accountID, provider: account.provider, model};
    const current = ((middlewareUI.model_rename||{})[account.provider]||{})[model] || '';
    $('#model-rename-desc').textContent = `账号 ${account.name||account.id}（${platformName(account.provider)}）：${model}`;
    $('#model-rename-input').value = current || model;
    $('#model-rename-clear').hidden = !current;
    $('#model-rename-error').textContent = '';
    $('#model-rename-dialog').showModal();
    $('#model-rename-input').select();
  }
  async function modelRenameSave(provider, model, target) {
    const next = JSON.parse(JSON.stringify(middlewareUI.model_rename || {}));
    const entries = next[provider] || {};
    if (target && target !== model) entries[model] = target; else delete entries[model];
    if (Object.keys(entries).length) next[provider] = entries; else delete next[provider];
    await api('/middleware/model_rename', {method:'PUT', body:{model_rename: next}});
    await loadMiddleware();
    if (page === 'models') render();
  }
  function closeDrawer() { selected=null; $('.app-shell').inert=false; $('#drawer-backdrop').hidden=true; $('#account-drawer').hidden=true; document.body.classList.remove('modal-open'); }
  // Platform catalog comes from the gateway (/admin/v1/info), so a new
  // registered provider shows up here without touching this file.
  let platforms = [];
  const platformOf = id => platforms.find(p => p.id===id) || {id, name:id, credential:'API Key', placeholder:'', hint:'', auth:'api_key', requires_uid:false, base_url:''};
  const platformName = id => platformOf(id).name;
  const platformLetters = id => (platformOf(id).name || id || '?').replace(/[^A-Za-z\u4e00-\u9fa5]/g,'').slice(0,1).toUpperCase() || '?';
  function platformTile(p, selected, locked) {
    const logo = p.logo ? `<svg class="platform-logo" aria-hidden="true"><use href="#${esc(p.logo)}"/></svg>` : `<span class="platform-logo fallback" aria-hidden="true">${esc(platformLetters(p.id))}</span>`;
    return `<button type="button" class="platform-card${selected?' selected':''}" role="radio" aria-checked="${selected}" data-provider="${esc(p.id)}" ${locked?'disabled aria-disabled="true"':''} title="${esc(p.tagline||p.name)}">${logo}<span class="platform-meta"><b>${esc(p.name)}</b><small>${esc(p.tagline||'')}</small></span>${locked&&selected?'<span class="platform-lock">已创建</span>':''}</button>`;
  }
  function renderPlatformPicker(current, locked) {
    $('#platform-picker').innerHTML = platforms.map(p => platformTile(p, p.id===current, locked)).join('');
    $('#account-provider').value = current || '';
    $('#platform-note').textContent = locked ? '账号创建后平台不可更换；如需换平台请新建账号。' : '选择要接入的平台，下方字段会随平台自动调整。';
  }
  function selectPlatform(prov) {
    $('#account-provider').value = prov;
    document.querySelectorAll('#platform-picker .platform-card').forEach(card => {
      const on = card.dataset.provider===prov;
      card.classList.toggle('selected', on); card.setAttribute('aria-checked', String(on));
    });
    setProviderUI(prov);
  }
  function setProviderUI(prov, keepBase=false) {
    const p = platformOf(prov);
    const wb = p.auth==='oauth' || p.requires_uid;
    $('#workbuddy-fields').hidden = !wb;
    const panel = $('#oauth-panel'); if (panel) panel.hidden = !wb;
    wbOAuthReset(true);
    // UID is required on create; on edit it is never echoed back, so leaving
    // it empty keeps the stored value (the API only applies non-empty input).
    $('#account-uid').required = wb && !editing;
    const keep = editing ? '<span class="optional">留空保留现有凭证</span>' : '<span>*</span>';
    $('#account-key-label').innerHTML = `${esc(p.credential||'API Key')} ${keep}`;
    $('#account-key').placeholder = p.placeholder || '';
    const hint = $('#account-key-hint');
    if (hint) hint.textContent = p.hint || '保存后不再显示明文，检查状态不会发起模型生成。';
    if (!keepBase) $('#account-base').value = p.base_url || '';
  }
  function openAccount(id = null) {
    editing=id; const a=accounts.find(a=>a.id===id); $('#account-form').reset(); $('#account-error').textContent='';
    const prov = a?.provider || platforms[0]?.id || 'sensenova';
    renderPlatformPicker(prov, !!a); setProviderUI(prov);
    $('#account-dialog-title').textContent = a?'编辑账号':'接入新的账号';
    $('#account-name').value=a?.name||''; $('#account-id').value=a?.id||`${platformOf(prov).id_prefix||prov.slice(0,5)}-${crypto.randomUUID().slice(0,8)}`; $('#account-id').disabled=!!a;
    $('#account-uid').value=a?.uid||''; $('#account-realm').value=a?.realm||'intl'; $('#account-product').value=a?.product||'desktop';
    $('#group-list').innerHTML=groups.map(g=>`<option value="${esc(g)}">`).join(''); $('#account-group').value=a?.group||''; $('#account-base').value=a?.base_url||platformOf(prov).base_url||''; $('#account-concurrency').value=a?.max_in_flight||4; $('#account-models').value=a?.models_allowlist?.join('\n')||''; $('#account-key').required=!a; $('#account-key').type='password'; $('#account-dialog .advanced').open=!!a; $('#account-dialog').showModal();
  }
  async function loadAccounts() { const result=await api('/accounts'); accounts=result.data||[]; groups=[...new Set(accounts.map(a=>a.group||'default'))].sort(); if(info.config_default_group)defaultGroup=info.config_default_group; if(page==='sessions')$('#nav-count').textContent=accounts.length;else render(); $('#last-update').textContent='上次同步 '+new Date().toLocaleTimeString('zh-CN',{hour12:false}); $('#error-banner').hidden=true; $('#connection-status').className='connection connected'; $('#connection-status').innerHTML='<i></i>'+esc(info.instance_name||'网关已连接'); }

  // ---- WorkBuddy browser OAuth login ----
  function wbOAuthStop() {
    if (wbOAuth.timer) { clearInterval(wbOAuth.timer); wbOAuth.timer=null; }
    wbOAuth.state=''; wbOAuth.active=false;
    const start=$('#oauth-start'); if(start) start.disabled=false;
    const cancel=$('#oauth-cancel'); if(cancel) cancel.hidden=true;
  }
  function wbOAuthReset(silent=false) {
    wbOAuthStop();
    if (!silent) { const s=$('#oauth-status'); if(s){ s.textContent=''; s.removeAttribute('data-state'); } }
  }
  async function wbOAuthStart() {
    if (wbOAuth.active) return;
    wbOAuth.active=true;
    const start=$('#oauth-start'), status=$('#oauth-status');
    start.disabled=true; status.textContent='正在请求官方授权链接…'; status.removeAttribute('data-state');
    try {
      const r=await api('/workbuddy/login/start',{method:'POST',body:{realm:$('#account-realm').value,platform:'CLI'}});
      wbOAuth.state=r.state;
      $('#oauth-cancel').hidden=false;
      status.textContent='官方登录页已打开，请完成登录（支持扫码）。完成后网关自动获取 UID / Access Token / Refresh Token 并创建账号。';
      window.open(r.auth_url,'_blank','noopener');
      wbOAuth.timer=setInterval(wbOAuthPoll,2500);
    } catch(error) {
      wbOAuthStop(); status.textContent=error.message; status.setAttribute('data-state','error');
    }
  }
  async function wbOAuthPoll() {
    if (!wbOAuth.state) return;
    const status=$('#oauth-status');
    let r;
    try { r=await api('/workbuddy/login/poll',{method:'POST',body:{state:wbOAuth.state}}); }
    catch(error) { wbOAuthStop(); status.textContent='授权轮询失败：'+error.message; status.setAttribute('data-state','error'); return; }
    if (r.status==='ok') {
      wbOAuthStop();
      const name=r.account?.name||r.account_id;
      $('#account-dialog').close();
      await loadAccounts();
      toast(`${r.updated?'账号凭证已更新':'账号已通过浏览器授权创建'}：${name}`);
      if (r.account_id) check(r.account_id, false);
    } else if (r.status==='pending') {
      status.textContent=`等待浏览器完成登录… ${r.message?`（${r.message}）`:''}`;
    } else {
      wbOAuthStop();
      status.textContent=r.status==='unknown'?'登录状态已失效，请重新开始授权。':r.status==='expired'?'登录窗口已过期（10 分钟），请重新开始授权。':(r.message||'授权未完成，请重试。');
      status.setAttribute('data-state','error');
    }
  }
  async function check(id, quiet=false) {
    if (checking.has(id)) return; checking.add(id); render();
    try { const result=await api(`/accounts/${encodeURIComponent(id)}/check`,{method:'POST'}); await loadAccounts(); const a=result.data; if (!quiet) toast(a?.discovery_error?`检查完成：${a.discovery_error}`:`${a?.name||id} · 已发现 ${a?.models?.length||0} 个模型`,!!a?.discovery_error); }
    catch(error) { if (!quiet) toast(error.message,true); }
    finally { checking.delete(id); render(); }
  }
  async function refreshAll() {
    const button=$('#refresh-all'); button.disabled=true; button.innerHTML=icon('refresh')+'刷新中…'; button.querySelector('svg').classList.add('spinning');
    try { const queue=[...accounts]; const worker=async()=>{while(queue.length) await check(queue.shift().id,true);}; await Promise.all([worker(),worker()]); await loadAccounts(); toast(accounts.length?'账号状态已更新':'已同步，添加账号后即可检查状态'); }
    catch(error) { toast(error.message,true); }
    finally { button.disabled=false; button.innerHTML=icon('refresh')+'刷新状态'; }
  }
  // No login step: GET /admin/ already set the HttpOnly session cookie, so the
  // console boots straight into the dashboard.
  async function connect() {
    try {
      info=await api('/info');
      platforms=Array.isArray(info.platforms)&&info.platforms.length?info.platforms:platforms;
      connected=true;
      await loadAccounts();
      if (page==='overview') { await Promise.all([loadGroupStats(), loadMiddleware()]); if (page==='overview') render(); }
    } catch(error) {
      connected=false;
      $('#error-banner').textContent='连接管理台失败：'+error.message;
      $('#error-banner').hidden=false;
      $('#connection-status').className='connection offline';
      $('#connection-status').innerHTML='<i></i> 连接中断';
    }
  }
  connect();
  $('#platform-picker').addEventListener('click',event=>{
    const card = event.target.closest('.platform-card');
    if (!card || card.disabled || editing) return;
    selectPlatform(card.dataset.provider);
  });
  $('#platform-picker').addEventListener('keydown',event=>{
    if (editing || (event.key!=='ArrowRight' && event.key!=='ArrowLeft' && event.key!=='ArrowDown' && event.key!=='ArrowUp')) return;
    event.preventDefault();
    const cards=[...document.querySelectorAll('#platform-picker .platform-card')];
    const i=cards.findIndex(c=>c.classList.contains('selected'));
    const next=cards[(i+(event.key==='ArrowRight'||event.key==='ArrowDown'?1:cards.length-1)+cards.length)%cards.length];
    selectPlatform(next.dataset.provider); next.focus();
  });
  document.addEventListener('change',event=>{
    // #provider-filter lives in the dynamically rendered accounts view; bind
    // through delegation like #account-search so the page can load without it.
    if (event.target && event.target.id==='provider-filter'){ providerFilter=event.target.value; render(); }
    if (event.target && (event.target.id==='group-filter' || event.target.id==='model-group-filter')){ groupFilter=event.target.value; render(); }
    if (event.target && event.target.id==='routing-group-filter'){ routingGroupFilter=event.target.value; render(); }
    if (event.target && event.target.id==='connect-group'){ connectGroup=event.target.value; render(); }
    if (event.target && event.target.id==='routing-strategy' && event.target.value!==middlewareUI.routing){ api('/routing',{method:'PUT',body:{strategy:event.target.value}}).then(()=>{ middlewareUI.routing=event.target.value; render(); toast('调度策略已热更新：'+(event.target.value==='smart'?'智能':'轮询')); }).catch(error=>toast(error.message,true)); }
  });
  $('#account-form').addEventListener('submit',async event=>{    event.preventDefault(); const button=$('#account-save'); button.disabled=true; $('#account-error').textContent='';
    const payload={id:$('#account-id').value.trim(),name:$('#account-name').value.trim(),provider:$('#account-provider').value,group:$('#account-group').value.trim(),base_url:$('#account-base').value.trim(),max_in_flight:Number($('#account-concurrency').value),models:$('#account-models').value.split(/[,，\n]+/).map(v=>v.trim()).filter(Boolean)};
    if (payload.provider==='workbuddy') {
      payload.uid=$('#account-uid').value.trim();
      payload.realm=$('#account-realm').value;
      payload.product=$('#account-product').value;
      const rt=$('#account-refresh').value.trim(); if (rt) payload.refresh_token=rt;
    }
    if ($('#account-key').value.trim()) payload.api_key=$('#account-key').value.trim();
    try { await api('/accounts'+(editing?'/'+encodeURIComponent(editing):''),{method:editing?'PUT':'POST',body:payload}); $('#account-dialog').close(); await loadAccounts(); toast('账号已加密保存，正在检查连接…'); await check(payload.id); }
    catch(error) { $('#account-error').textContent=error.message; }
    finally { button.disabled=false; }
  });
  $('#quota-form').addEventListener('submit',async event=>{
    event.preventDefault(); const a=accounts.find(a=>a.id===quotaID); if(!a)return;
    const pool=$('#quota-pool').value, now=new Date(), ttl=Number($('#quota-ttl').value), models=pool==='flash-lite'?(a.models.filter(m=>m.id.includes('flash-lite')).map(m=>m.id)):[];
    const buckets=[['5h',Number($('#quota-five').value)],['week',Number($('#quota-week').value)]].map(([window,remaining])=>({name:`${pool}-${window}`,pool,unit:'credits',remaining,limit:0,...(pool==='flash-lite'?{models:models.length?models:['sensenova-6.8-flash-lite']}: {})}));
    try { await api(`/accounts/${encodeURIComponent(quotaID)}/quota`,{method:'PUT',body:{source:'manual-console',partial:true,observed_at:now.toISOString(),valid_until:new Date(now.getTime()+ttl*60000).toISOString(),remaining_tokens:null,buckets}}); $('#quota-dialog').close(); await loadAccounts(); toast('额度观测已更新'); }
    catch(error){$('#quota-error').textContent=error.message;}
  });
  $('#agent-form').addEventListener('submit',async event=>{
    event.preventDefault(); const button=$('#agent-save'); button.disabled=true; $('#agent-error').textContent='';
    const row=agentsUI.bind; if(!row){ button.disabled=false; return; }
    const model=$('#agent-model').value;
    if(!model){ $('#agent-error').textContent='请选择模型'; button.disabled=false; return; }
    try {
      const payload=agentDialogPayload();
      const result=await api(`/agents/${encodeURIComponent(row.id)}/bind`,{method:'POST',body:payload});
      $('#agent-dialog').close();
      await loadAgents();
      toast(result.wrote?`已写入 ${row.name||row.id} 的配置${result.backed_up?'（已备份原文件）':''}`:`已生成 ${row.name||row.id} 的接入片段`);
    } catch(error){ $('#agent-error').textContent=error.message; }
    finally { button.disabled=false; }
  });
  $('#confirm-form').addEventListener('submit',async event=>{
    event.preventDefault(); const button=$('#confirm-delete'); button.disabled=true;
    try { await api(`/accounts/${encodeURIComponent(deleteID)}`,{method:'DELETE'}); $('#confirm-dialog').close(); closeDrawer(); await loadAccounts(); toast('账号已移除'); }
    catch(error){$('#confirm-error').textContent=error.message;}
    finally {button.disabled=false;}
  });
  function openRenameGroup(g) {
    groupAction = {kind:'rename', name:g};
    $('#group-name').value = '';
    $('#group-rename-error').textContent = '';
    $('#group-rename-title').textContent = '重命名分组 ' + g;
    $('#group-rename-note').textContent = '把「' + g + '」改为新名字：组内账号会整体移动，轮询游标重置；若它是默认分组，默认分组也会跟着改。';
    $('#group-rename-dialog').showModal();
  }
  function openDeleteGroup(g) {
    groupAction = {kind:'delete', name:g};
    const targets = [...new Set(groups.filter(x=>x!==g).concat(defaultGroup))];
    $('#group-move-to').innerHTML = targets.map(t=>`<option value="${esc(t)}">${esc(t)}</option>`).join('');
    const isDefault = g === defaultGroup;
    $('#group-delete-desc').textContent = isDefault ? '「' + g + '」是默认分组，不能解散。可以先重命名它，或把其他分组设为默认。' : '解散分组「' + g + '」：组内 ' + number(groupStat(g).accounts) + ' 个账号将移入所选分组。';
    $('#group-delete-save').disabled = isDefault;
    $('#group-delete-error').textContent = '';
    $('#group-delete-dialog').showModal();
  }
  async function aliasSave(next) {
    await api('/middleware/model_map', {method:'PUT', body:{model_map: next}});
    await loadMiddleware(); if (page==='models') render();
  }
document.addEventListener('click',async event=>{
    const nav=event.target.closest('[data-page]'); if(nav){if(page!==nav.dataset.page)clearClientKey(); if(page!==nav.dataset.page)++sessionEpoch; page=nav.dataset.page; render();if(page==='sessions')await loadSessions();else if(page==='redaction'){await loadRedaction();if(redaction?.enabled)await loadRedactionSpans();}else if(page==='agents')await loadAgents();else if(page==='routing'){await loadRouting();await loadMiddleware();}else if(page==='usage')await loadUsage();else if(page==='overview'){await Promise.all([loadGroupStats(), loadMiddleware()]); if(page==='overview')render();}else if(page==='models')await loadMiddleware();}
    const reveal=event.target.closest('.reveal'); if(reveal){const input=$('#'+reveal.dataset.target); input.type=input.type==='password'?'text':'password';}
    const close=event.target.closest('.close-dialog'); if(close)close.closest('dialog').close();
    const action=event.target.closest('[data-action]'); if(!action)return; const id=action.dataset.id;
    try { if(action.dataset.action.startsWith('session-')){await sessionAction(action.dataset.action,action);return;} switch(action.dataset.action){
      case 'add': openAccount(); break;
      case 'edit': openAccount(id); break;
      case 'check': await check(id); break;
      case 'details': openDrawer(id); break;
      case 'close-drawer': closeDrawer(); break;
      case 'drawer-tab': drawerTab=action.dataset.tab; renderDrawer(); break;
      case 'model-rename': openModelRename(action.dataset.id, action.dataset.model); break;
      case 'model-rename-reset': {
        if (!modelRename) break;
        const button = $('#model-rename-clear'); button.disabled = true;
        try { await modelRenameSave(modelRename.provider, modelRename.model, ''); $('#model-rename-dialog').close(); renderDrawer(); toast('已还原模型名'); }
        catch (error) { $('#model-rename-error').textContent = error.message; }
        finally { button.disabled = false; }
        break;
      }
      case 'filter': filter=action.dataset.filter; render(); break;
      case 'clear-filter': filter='all'; search=''; render(); break;
      case 'toggle': {const a=accounts.find(a=>a.id===id); action.disabled=true; await api(`/accounts/${encodeURIComponent(id)}/enabled`,{method:'PUT',body:{enabled:!a.enabled}}); await loadAccounts(); toast(a.enabled?'账号已停用':'账号已启用');break;}
      case 'delete': deleteID=id; $('#confirm-description').textContent=`移除“${accounts.find(a=>a.id===id)?.name||id}”后，它将不再参与调度。可使用原 API Key 重新添加。配置文件中的同名账号也会被排除。`; $('#confirm-error').textContent=''; $('#confirm-dialog').showModal(); break;
      case 'quota': quotaID=id; $('#quota-form').reset(); $('#quota-error').textContent=''; $('#quota-dialog').showModal(); break;
      case 'clear-quota': await api(`/accounts/${encodeURIComponent(id)}/quota`,{method:'DELETE'}); await loadAccounts(); toast('额度快照已清除'); break;
      case 'wb-task': {
        cwTaskBusy=true; renderDrawer();
        try {
          const res=await api(`/accounts/${encodeURIComponent(id)}/tasks/${encodeURIComponent(action.dataset.kind)}`,{method:'POST',body:{}});
          await loadAccounts();
          toast(res.message||'任务已完成');
        } catch(error) { toast(error.message,true); }
        finally { cwTaskBusy=false; if(selected) renderDrawer(); }
        break;
      }
      case 'agents-refresh': await loadAgents(); break;
      case 'agents-mode': agentsUI.mode=action.dataset.mode; await api('/agents',{method:'PUT',body:{write_mode:action.dataset.mode}}); await loadAgents(); toast(`默认写入模式：${action.dataset.mode==='auto'?'自动写入':'仅生成片段'}`); break;
      case 'agents-bind': openAgentDialog(id); break;
      case 'agents-check': { agentsUI.busy=true; render(); try { const res=await api(`/agents/${encodeURIComponent(id)}/check`,{method:'POST',body:{}}); toast(res.ok?'自检通过：'+res.note:'自检失败：'+res.note, !res.ok); await loadAgents(); } catch(error){ toast(error.message,true); await loadAgents(); } break; }
      case 'agents-restore': { try { await api(`/agents/${encodeURIComponent(id)}/restore`,{method:'POST',body:{}}); toast('配置已还原到备份版本'); await loadAgents(); } catch(error){ toast(error.message,true); } break; }
      case 'agent-preview': await agentPreview(); break;
      case 'routing-refresh': await loadRouting(); break;
      case 'usage-refresh': await loadUsage(); break;
      case 'usage-window': usageUI.window=action.dataset.window; await loadUsage(); break;
      case 'profiles-save': {
        const name = $('#profile-name').value.trim();
        if (!name) { toast('请输入方案名称',true); break; }
        const agents = {};
        agentsUI.rows.forEach(a => { if (a.binding?.model) agents[a.id] = a.binding.model; });
        if (!Object.keys(agents).length) { toast('先为至少一个 Agent 选择模型',true); break; }
        try { await api('/agents/profiles',{method:'PUT',body:{name,agents}}); await loadAgents(); toast('方案已保存：'+name); }
        catch(error) { toast(error.message,true); }
        break;
      }
      case 'profiles-apply': {
        try {
          const res = await api(`/agents/profiles/${encodeURIComponent(action.dataset.name)}/apply`,{method:'POST',body:{}});
          const failed = (res.results||[]).filter(r=>!r.ok);
          toast(failed.length ? `方案 ${res.profile} 应用失败：${failed.map(f=>f.agent).join('、')}` : `方案 ${res.profile} 已应用`);
          await loadAgents();
        } catch(error) { toast(error.message,true); await loadAgents(); }
        break;
      }
      case 'agents-version': {
        try { const res=await api(`/agents/${encodeURIComponent(id)}/version`,{method:'POST',body:{}}); agentsUI.versions[id]=res.version; render(); toast(res.version.detected?'已探测到版本：'+res.version.version:(res.version.error||'未检测到')); }
        catch(error){ toast(error.message,true); }
        break;
      }
      case 'profiles-delete': {
        try { await api(`/agents/profiles/${encodeURIComponent(action.dataset.name)}`,{method:'DELETE'}); await loadAgents(); toast('方案已删除'); }
        catch(error) { toast(error.message,true); }
        break;
      }
      case 'toggle-client-key': if(clientKey){clearClientKey(); render();}else await useClientKey(key=>{clientKey=key;}); break;
      case 'copy-client-key': await useClientKey(async key=>{await navigator.clipboard.writeText(key); toast('代理访问 Key 已复制');}); break;
      case 'copy-code': await useClientKey(async key=>{await navigator.clipboard.writeText(connectionCode(key)); toast('调用示例已复制，已包含代理访问 Key');}); break;
      case 'group-jump': groupFilter = action.dataset.group; render(); break;
      case 'routing-group': routingGroupFilter = action.dataset.group; render(); break;
      case 'group-default': { await api('/groups/default', {method:'PUT', body:{name: action.dataset.group}}); defaultGroup = action.dataset.group; await Promise.all([loadAccounts(), loadGroupStats()]); if (page==='overview') render(); toast('默认分组已改为：'+action.dataset.group); break; }
      case 'group-strategy': {
        const g=action.dataset.group;
        const cur=groupStat(g).routing_override || middlewareUI.routing || 'round_robin';
        const next=cur==='smart'?'round_robin':'smart';
        try {
          await api('/groups/'+encodeURIComponent(g)+'/routing',{method:'PUT',body:{strategy:next}});
          await Promise.all([loadGroupStats(), loadMiddleware()]);
          if(page==='overview')render();
          toast(`分组 ${g} 调度策略已改为${next==='smart'?'智能':'轮询'}`);
        } catch(error){ toast(error.message,true); }
        break;
      }
      case 'group-rename': openRenameGroup(action.dataset.group); break;
      case 'group-delete': openDeleteGroup(action.dataset.group); break;
      case 'group-accounts': groupFilter = action.dataset.group; page = 'accounts'; render(); break;
      case 'model-detail': modelDetail = modelDetail === action.dataset.id ? '' : action.dataset.id; render(); break;
      case 'alias-add': { const client = $('#alias-client').value.trim(); const upstream = $('#alias-target').value; if (!client || !upstream) { toast('请填写客户端模型名并选择目标模型', true); break; } const next = Object.assign({}, middlewareUI.model_map || {}); next[client] = upstream; await aliasSave(next); toast('别名已保存：'+client+' → '+upstream); break; }
      case 'alias-del': { const next = Object.assign({}, middlewareUI.model_map || {}); delete next[action.dataset.client]; await aliasSave(next); toast('别名已删除'); break; }
      case 'copy-text': await navigator.clipboard.writeText(action.dataset.text); toast('已复制'); break;
      case 'redaction-refresh': await loadRedaction(); break;
      case 'redaction-tab': redactionTab=action.dataset.tab; if(redactionTab==='log'&&!redactionSpans) await loadRedactionSpans(); else render(); break;
      case 'redaction-kind': redactionKind=action.dataset.kind; await loadRedactionSpans(); break;
      case 'redaction-spans-refresh': await loadRedactionSpans(); break;
      case 'redaction-toggle-rule': { const label=action.dataset.label; const disabled=new Set(redaction.disabled_rules||[]); disabled.has(label)?disabled.delete(label):disabled.add(label); await api('/redaction/rules',{method:'PUT',body:{disabled_rules:[...disabled]}}); await loadRedaction(); toast(disabled.has(label)?`规则 ${label} 已关闭`:`规则 ${label} 已开启`); break; }
      case 'redaction-toggle-toolguard': { const guard=redaction.tool_guard||{mode:'observe'}; const enabled=!(guard.enabled===true); await api('/redaction/rules',{method:'PUT',body:{tool_guard:{enabled,mode:guard.mode||'observe'}}}); await loadRedaction(); toast(enabled?'危险工具检测已开启（observe）':'危险工具检测已关闭'); break; }
      case 'redaction-save-words': { const raw=$('#redaction-words').value.trim(); let custom_words={}; if(raw) { try { custom_words=JSON.parse(raw); } catch(error) { toast('自定义词必须是合法 JSON',true); return; } } await api('/redaction/rules',{method:'PUT',body:{custom_words}}); await loadRedaction(); toast('规则已热更新'); break; }
    }}catch(error){toast(error.message,true);}
  });
  document.addEventListener('input',event=>{if(event.target.id==='account-search'){const position=event.target.selectionStart; search=event.target.value; $('#page-content').innerHTML=accountsView(); const field=$('#account-search'); field.focus(); field.setSelectionRange(position,position);}});
  $('#refresh-all').addEventListener('click',refreshAll); $('#add-account').addEventListener('click',()=>openAccount()); $('#drawer-backdrop').addEventListener('click',closeDrawer);
  $('#oauth-start').addEventListener('click',wbOAuthStart); $('#oauth-cancel').addEventListener('click',()=>{wbOAuthReset(); const s=$('#oauth-status'); if(s)s.textContent='已取消授权';});
  document.querySelectorAll('dialog').forEach(dialog=>dialog.addEventListener('close',()=>{if(dialog.id==='account-dialog'){$('#account-key').value='';$('#account-key').type='password';wbOAuthReset(true);}}));
  document.addEventListener('keydown',event=>{if(event.key==='Escape'&&!document.querySelector('dialog[open]'))closeDrawer();});
  setInterval(async()=>{if(!connected||document.hidden||polling||document.querySelector('dialog[open]'))return; polling=true; try{if(page==='overview')await loadGroupStats(); await loadAccounts();}catch(error){$('#error-banner').textContent='同步失败：'+error.message;$('#error-banner').hidden=false;$('#connection-status').className='connection offline';$('#connection-status').innerHTML='<i></i> 同步中断';}finally{polling=false;}},10000);
  render();
})();
