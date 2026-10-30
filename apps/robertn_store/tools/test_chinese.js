'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const here = path.join(__dirname, '..');
const fixture = require('./fixture.js');
const { stage } = require('./localize.js');
const { frame: englishFrame, placement } = require('../src/store_frame.js');
const { HmiText, COUNT, slot } = require('../src/hmi_text.js');
const dir = fs.mkdtempSync(path.join(here, 'build-zh-test-'));
const m = stage(here, dir);
const zh = (name) => require(path.join(dir, 'src', name));
const { t, chinese, credit } = zh('i18n.js');
const fonts = zh('kit_fonts.js');
const { frame, placement: zhPlacement } = zh('store_frame.js');
const { view } = zh('store_ui.js');
const { Recorder, atlasId, play } = zh('kit_record.js');
const { Canvas } = zh('kit_canvas.js');
let screens = 0;
assert(chinese);
assert.strictEqual(t('Store'), '应用商店');
assert.strictEqual(t('Unknown text'), 'Unknown text');
assert.strictEqual(t('Added {0}', 'Store/{0}<script>'), '已添加 Store/{0}<script>');
assert.deepStrictEqual(zh('store_view.js').parseSource('https://github.com/Owner/Store/tree/Installed'),
    { type: 'github', repo: 'Owner/Store', channel: 'branch:Installed' });
const words = zh('locale_zh.js');
for (const [key, value] of Object.entries(words)) {
    // Every placeholder remains available, except English plural suffixes.
    const expected = key.match(/\{\d+\}/g) || [];
    for (const arg of expected) if (arg !== '{1}' || !/update|source/.test(key)) assert(value.includes(arg), key);
    const f = fonts.get('label', 28, value);
    for (const ch of value) if (/[^\x20-\x7e]/.test(ch)) assert(f.glyphs[ch.codePointAt(0)], `Missing glyph: ${ch}`);
}
const f = fonts.get('label', 22, credit);
for (const ch of credit) assert(f.glyphs[ch.codePointAt(0)], ch);
assert(f.atlasW <= 1024 && f.atlasH <= 2048);
assert.notStrictEqual(atlasId(f, 'label'), atlasId(fonts.get('label', 22, 'Store'), 'label'));
const cv = new Canvas(1200, 100), rec = new Recorder(1200, 100);
assert(Math.abs(cv.textWidth(credit, 22) - rec.textWidth(credit, 22)) < 0.001);
rec.text(credit, 0, 40, 22, '#ffffff');
const rendered = play(rec.words(), cv);
assert(rendered.png().length > 1000);
for (const [w, h] of [[1796, 632], [1790, 638], [2880, 764]]) {
    for (const [name, over, stOver] of fixture.screens()) {
        const state = stOver ? Object.assign(fixture.state(), stOver) : undefined;
        const model = fixture.model(over, state);
        const before = JSON.stringify(model), p = zhPlacement(w, h), ep = placement(w, h);
        const got = frame(model, { textScale: p.scale, layout: p.layout, scroll: {}, focus: model.focus });
        const english = englishFrame(model, { textScale: ep.scale, layout: ep.layout, scroll: {}, focus: model.focus });
        assert(!got.rec.errors, name);
        assert.strictEqual(JSON.stringify(model), before, `Mutated updater state: ${name}`);
        assert.deepStrictEqual(got.items.map((i) => i.act), english.items.map((i) => i.act), `Changed requests/focus order: ${name}`);
        screens++;
    }
}
const st = fixture.state();
st.components[0].title = 'Installed';
st.components[0].description = 'Store';
const values = view(fixture.model({ stack: [{ name: 'main' }, { name: 'app', id: st.components[0].id }] }, st)).values;
assert.strictEqual(values.name.text, 'Installed');
assert.strictEqual(values.desc.text, 'Store');
for (const hmi of ['ID7', 'ID8']) {
    const tree = path.join(dir, m.tree, 'rhmiData');
    const ui = JSON.parse(fs.readFileSync(path.join(tree, 'uiDescriptions', hmi, 'uiDescription.json')));
    const strings = JSON.parse(fs.readFileSync(path.join(tree, 'variantGroups/Common/texts/en_GB.json')));
    assert(Object.keys(strings).length <= 1024);
    for (let i = 0; i < COUNT; i++) assert(slot(i) in strings);
    let ids = 0;
    function walk(x) {
        if (!x || typeof x !== 'object') return;
        if (x.type === 'StringId') { assert(x.value in strings, x.value); ids++; }
        Object.keys(x).forEach((k) => walk(x[k]));
    }
    walk(ui);
    assert(ids > 30);
    assert(fs.readFileSync(path.join(tree, 'uiDescriptions', hmi, 'application.xml'), 'utf8').includes('id="9500"'));
}
async function hmi() {
    const out = [], bundle = { setStrings(v) { out.push(v); return Promise.resolve(); } };
    const component = { getStringBundle: () => bundle, setValue(...args) { out.push(args); return Promise.resolve(); } };
    const text = new HmiText();
    await text.set(component, 'status', 0, 'Text', '检查中');
    await text.set(component, 'status', 0, 'Text', '已安装');
    assert.strictEqual(text.slots.size, 1);
    assert.strictEqual(out[1][3], 'StringId');
    assert.strictEqual(out[3][2], slot(0));
    await text.set(component, 'url', 0, 'Text', 'https://host/#t=0123');
    assert.strictEqual(out[out.length - 1][3], 'Text');
    console.log(`Chinese: ${screens} frames, unchanged actions/data, placeholder and glyph coverage, software/GPU metrics, StringId resources and updates pass`);
}
hmi().catch((err) => { console.error(err); process.exitCode = 1; });
