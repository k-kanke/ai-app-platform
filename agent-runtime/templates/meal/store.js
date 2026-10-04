'use strict';
// JSON-file backed store. Last write wins (the latest input is the source of truth).
const fs = require('node:fs');
const path = require('node:path');

const VALID = new Set(['yes', 'no', 'none']);
const SLOTS = new Set(['lunch', 'dinner']);

function createStore(dataDir) {
  fs.mkdirSync(dataDir, { recursive: true });
  const file = path.join(dataDir, 'meals.json');
  let state = { members: ['妹', '孝太郎', '母', '父'], answers: {}, updatedAt: null };

  if (fs.existsSync(file)) {
    state = { ...state, ...JSON.parse(fs.readFileSync(file, 'utf8')) };
  }

  function persist() {
    const tmp = `${file}.${process.pid}.tmp`;
    fs.writeFileSync(tmp, JSON.stringify(state));
    fs.renameSync(tmp, file); // atomic: a crash never leaves a half-written file
  }

  const key = (member, date, slot) => `${date}|${slot}|${member}`;

  return {
    members: () => state.members,
    get() {
      return { members: state.members, answers: state.answers, updatedAt: state.updatedAt };
    },
    set(member, date, slot, value) {
      if (!state.members.includes(member)) throw new Error('unknown member');
      if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) throw new Error('invalid date');
      if (!SLOTS.has(slot)) throw new Error('invalid slot');
      if (!VALID.has(value)) throw new Error('invalid value');
      const k = key(member, date, slot);
      if (value === 'none') delete state.answers[k];
      else state.answers[k] = value;
      state.updatedAt = new Date().toISOString();
      persist();
    },
  };
}

module.exports = { createStore };
