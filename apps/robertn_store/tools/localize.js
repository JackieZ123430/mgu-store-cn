'use strict';
// Build a fixed-language source package in an isolated tree. The checked-in
// package, English output and updater protocol remain the default.
const fs = require('fs');
const path = require('path');
const link = require('../../../tools/oap_link.js');
const words = require('../src/locale_zh.js');
const { COUNT, slot } = require('../src/hmi_text.js');

function copy(from, to) {
    if (fs.statSync(from).isDirectory()) {
        fs.mkdirSync(to, { recursive: true });
        fs.readdirSync(from).forEach((name) => copy(path.join(from, name), path.join(to, name)));
    } else {
        fs.mkdirSync(path.dirname(to), { recursive: true });
        fs.copyFileSync(from, to);
    }
}
function json(file, value) { fs.writeFileSync(file, JSON.stringify(value, null, 2) + '\n'); }
function stage(here, dir) {
    const manifest = link.readManifest(here);
    copy(path.join(here, 'mgu-app.json'), path.join(dir, 'mgu-app.json'));
    manifest.modules.forEach((file) => copy(path.join(here, file), path.join(dir, file)));
    copy(path.join(here, manifest.tree), path.join(dir, manifest.tree));
    json(path.join(dir, 'mgu-app.json'), manifest);
    fs.writeFileSync(path.join(dir, 'src/language.js'), "'use strict';\nmodule.exports = 'zh-CN';\n");
    const tree = path.join(dir, manifest.tree);
    ['package.json', 'npm-shrinkwrap.json'].forEach((name) => {
        const file = path.join(tree, name), value = JSON.parse(fs.readFileSync(file, 'utf8'));
        value.version = manifest.version;
        json(file, value);
    });
    const appFile = path.join(tree, 'rhmiData/application.json');
    const app = JSON.parse(fs.readFileSync(appFile, 'utf8'));
    app.application.version = manifest.version;
    json(appFile, app);
    const textsDir = path.join(tree, 'rhmiData/variantGroups/Common/texts');
    const strings = JSON.parse(fs.readFileSync(path.join(textsDir, 'en_GB.json'), 'utf8'));
    strings.SID_STORE_TITLE = words.Store;
    let n = 0;
    function unicode(value) {
        if (!value || typeof value !== 'object') return;
        if (value.type === 'Text' && typeof value.value === 'string' && value.value) {
            const id = 'SID_STORE_STATIC_' + n++;
            strings[id] = words[value.value] || value.value;
            value.value = id;
            value.type = 'StringId';
        } else Object.keys(value).forEach((k) => unicode(value[k]));
    }
    ['ID7', 'ID8'].forEach((hmi) => {
        const file = path.join(tree, 'rhmiData/uiDescriptions', hmi, 'uiDescription.json');
        const ui = JSON.parse(fs.readFileSync(file, 'utf8'));
        const main = Object.values(ui.containers).find((c) => c.name === 'ST_OnlineApps_Store');
        main.components[9500] = { type: 'label', name: 'storeTranslationCredit',
            properties: { Visible: { value: 1, type: 'UInt' } },
            rows: [{ Text: { value: '汉化：抖音 小白科技杂物社 / BiliBili 小白151 / GitHub JackieZ123430', type: 'Text' } }] };
        const xmlFile = path.join(tree, 'rhmiData/uiDescriptions', hmi, 'application.xml');
        const xml = fs.readFileSync(xmlFile, 'utf8').replace(/(<container id="1"[\s\S]*?)(<\/layoutGroup>\s*<\/container>)/,
            '$1<component id="9500" type="label"><property id="2" value="1"/></component>$2');
        if (!xml.includes('id="9500"')) throw new Error('Start screen layout not found');
        fs.writeFileSync(xmlFile, xml);
        // Titles contain a wrapped Text cell as well as normal widget cells.
        function titles(x) {
            if (!x || typeof x !== 'object') return;
            if (x.Text && x.Text.type === undefined && x.Text.value && x.Text.value.type === 'Text') {
                unicode(x.Text.value);
            }
            Object.keys(x).forEach((k) => titles(x[k]));
        }
        unicode(ui);
        titles(ui);
        json(file, ui);
    });
    for (let i = 0; i < COUNT; i++) strings[slot(i)] = '';
    if (Object.keys(strings).length > 1024) throw new Error('RHMI StringBundle exceeds 1024 entries');
    json(path.join(textsDir, 'en_GB.json'), strings);
    return manifest;
}
function build(here, donor) {
    const dir = fs.mkdtempSync(path.join(here, 'build-zh-'));
    // Keep the staging folder for inspection; it contains no donor/SDK code.
    const m = stage(here, dir);
    const file = path.join(here, `${m.id}@${m.version}.zh-CN.src.tgz`);
    const members = link.packSource(dir, file).length;
    if (donor) link.build(dir, path.join(here, `${m.id}@${m.version}.zh-CN.tgz`), { donors: [donor], epoch: process.env.SOURCE_DATE_EPOCH });
    return { source: path.basename(file), sourceMembers: members, language: 'zh-CN', stage: dir };
}
module.exports = { stage, build };
