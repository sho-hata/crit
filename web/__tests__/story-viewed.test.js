'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const app = fs.readFileSync(path.join(__dirname, '..', 'app.js'), 'utf8');
const functions = ['storyRawHunks', 'viewedHunkKeys', 'areHunksViewed', 'reconcileStoryViewed']
  .map(name => app.match(new RegExp('  function ' + name + '\\([\\s\\S]*?\\n  }'))[0]).join('\n');
const { viewedHunkKeys, areHunksViewed, reconcileStoryViewed, setDiffCommit } = new Function(`
  const storyHidden = false, session = { story: {} };
  let diffCommit = '';
  function storyHasContent() { return true; }
  ${functions}
  return { viewedHunkKeys, areHunksViewed, reconcileStoryViewed, setDiffCommit: function(c) { diffCommit = c; } };
`)();

function hunk(start, content) {
  return { OldStart: start, Lines: [
    { Type: 'context', Content: 'context', OldNum: start, NewNum: start },
    { Type: 'add', Content: content, OldNum: 0, NewNum: start + 1 },
  ] };
}

test('hunk keys survive synthesized context and new-side line shifts, but not changed edits', () => {
  const raw = hunk(10, 'first');
  const expanded = structuredClone(raw);
  expanded.Lines[1].NewNum += 5;
  expanded.Lines.push({ Type: 'context', Content: 'expanded context' });
  assert.deepEqual(viewedHunkKeys([raw]), viewedHunkKeys([expanded]));
  assert.notDeepEqual(viewedHunkKeys([raw]), viewedHunkKeys([hunk(10, 'changed')]));
  assert.notDeepEqual(viewedHunkKeys([raw]), viewedHunkKeys([hunk(20, 'first')]));
});

test('merged visual hunks retain every raw hunk identity', () => {
  const raw = [hunk(1, 'first'), hunk(10, 'second')];
  const merged = { OldStart: 1, _StoryRawHunks: raw };
  assert.deepEqual(viewedHunkKeys([merged]), viewedHunkKeys(raw));
  const file = { viewedHunks: viewedHunkKeys([raw[0]]) };
  assert.equal(areHunksViewed(file, [merged]), false);
  file.viewedHunks = viewedHunkKeys(raw);
  assert.equal(areHunksViewed(file, [merged]), true);
});

test('refresh retains unchanged hunks and clears whole-file completion when an edit changes', () => {
  const raw = [hunk(1, 'first'), hunk(30, 'second'), hunk(60, 'third')];
  const file = { viewed: true, diffHunks: raw, viewedHunks: viewedHunkKeys(raw) };
  file.diffHunks = [raw[0], hunk(30, 'changed'), raw[2]];
  reconcileStoryViewed(file);
  assert.equal(file.viewed, false);
  assert.equal(areHunksViewed(file, [raw[0], raw[2]]), true);
  assert.equal(areHunksViewed(file, [file.diffHunks[1]]), false);
  assert.equal(areHunksViewed(file, []), false);
});

test('legacy whole-file flags cover all hunks; partial state stays pending while lazy', () => {
  const raw = [hunk(1, 'first'), hunk(30, 'second')];
  const file = { viewed: true, diffHunks: raw };
  reconcileStoryViewed(file);
  assert.deepEqual(file.viewedHunks, viewedHunkKeys(raw));
  file.lazy = true;
  file.diffHunks = [];
  reconcileStoryViewed(file);
  assert.deepEqual(file.viewedHunks, viewedHunkKeys(raw));
  assert.equal(file.viewed, true);
});

test('files without hunks (renames, binaries, failed diff loads) keep their file flag', () => {
  const file = { viewed: true, diffHunks: [] };
  reconcileStoryViewed(file);
  assert.equal(file.viewed, true);
  assert.equal(file.viewedHunks, undefined);

  const raw = [hunk(1, 'first')];
  const failed = { viewed: true, diffHunks: [], viewedHunks: viewedHunkKeys(raw) };
  reconcileStoryViewed(failed);
  assert.equal(failed.viewed, true);
});

test('a commit-scoped diff does not trim saved hunk keys', () => {
  const raw = [hunk(1, 'first'), hunk(30, 'second')];
  const file = { viewed: false, diffHunks: [raw[0]], viewedHunks: viewedHunkKeys([raw[1]]) };
  setDiffCommit('abc123');
  try {
    reconcileStoryViewed(file);
  } finally {
    setDiffCommit('');
  }
  assert.deepEqual(file.viewedHunks, viewedHunkKeys([raw[1]]));
  assert.equal(file.viewed, false);
});
