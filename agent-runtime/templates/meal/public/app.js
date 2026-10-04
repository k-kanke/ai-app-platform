'use strict';
const DAYS = 10;
const SYMBOL = { yes: '○', no: '×', none: '未' };
const NEXT = { none: 'yes', yes: 'no', no: 'none' };
const WD = ['日', '月', '火', '水', '木', '金', '土'];

let data = { members: [], answers: {} };
let me = localStorage.getItem('aap-meal-me');
let view = 'mine';

const ymd = (d) =>
  `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
const days = () =>
  Array.from({ length: DAYS }, (_, i) => {
    const d = new Date();
    d.setDate(d.getDate() + i);
    return d;
  });
const val = (m, date, slot) => data.answers[`${date}|${slot}|${m}`] || 'none';
const label = (d) => `${d.getMonth() + 1}/${d.getDate()} (${WD[d.getDay()]})`;

async function load() {
  try {
    const r = await fetch('/api/meals');
    data = await r.json();
    document.getElementById('err').hidden = true;
  } catch {
    const e = document.getElementById('err');
    e.hidden = false;
    e.textContent = '通信できませんでした。電波の良いところでもう一度試してください。';
  }
  if (!me || !data.members.includes(me)) me = null;
  render();
}

async function toggle(date, slot) {
  const next = NEXT[val(me, date, slot)];
  data.answers[`${date}|${slot}|${me}`] = next; // optimistic
  render();
  try {
    const r = await fetch('/api/meals', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ member: me, date, slot, value: next }),
    });
    data = await r.json();
  } catch {
    await load();
  }
  render();
}

function render() {
  const who = document.getElementById('who');
  who.innerHTML = '';
  data.members.forEach((m) => {
    const b = document.createElement('button');
    b.textContent = m;
    b.className = m === me ? 'on' : '';
    b.onclick = () => { me = m; localStorage.setItem('aap-meal-me', m); render(); };
    who.appendChild(b);
  });
  document.getElementById('tab-mine').className = view === 'mine' ? 'on' : '';
  document.getElementById('tab-all').className = view === 'all' ? 'on' : '';

  const main = document.getElementById('main');
  main.innerHTML = '';
  if (view === 'mine') {
    if (!me) { main.textContent = '上から自分の名前を選んでください。'; return; }
    days().forEach((d) => {
      const date = ymd(d);
      const card = document.createElement('div');
      card.className = 'day';
      card.innerHTML = `<h2>${label(d)}</h2>`;
      [['lunch', '昼'], ['dinner', '夜']].forEach(([slot, jp]) => {
        const row = document.createElement('div');
        row.className = 'row';
        const v = val(me, date, slot);
        row.innerHTML = `<span class="l">${jp}</span>`;
        const c = document.createElement('button');
        c.className = `cell ${v}`;
        c.textContent = SYMBOL[v];
        c.onclick = () => toggle(date, slot);
        row.appendChild(c);
        card.appendChild(row);
      });
      main.appendChild(card);
    });
  } else {
    const wrap = document.createElement('div');
    wrap.className = 'wrap';
    const t = document.createElement('table');
    let h = '<tr><th></th>';
    days().forEach((d) => (h += `<th colspan="2">${label(d)}</th>`));
    h += '</tr><tr><th></th>' + days().map(() => '<th>昼</th><th>夜</th>').join('') + '</tr>';
    let body = '';
    data.members.forEach((m) => {
      body += `<tr><th>${m}</th>`;
      days().forEach((d) =>
        ['lunch', 'dinner'].forEach((s) => {
          const v = val(m, ymd(d), s);
          body += `<td class="${v === 'none' ? 'miss' : ''}">${SYMBOL[v]}</td>`;
        }));
      body += '</tr>';
    });
    body += '<tr><th>食べる人数</th>' + days().map((d) =>
      ['lunch', 'dinner'].map((s) => `<td>${data.members.filter((m) => val(m, ymd(d), s) === 'yes').length}</td>`).join('')).join('') + '</tr>';
    t.innerHTML = h + body;
    wrap.appendChild(t);
    main.appendChild(wrap);
  }
}

document.getElementById('tab-mine').onclick = () => { view = 'mine'; render(); };
document.getElementById('tab-all').onclick = () => { view = 'all'; render(); };
load();
setInterval(load, 15000);
