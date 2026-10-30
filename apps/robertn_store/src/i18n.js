'use strict';
// 中文汉化：抖音 小白科技杂物社 / BiliBili 小白151 / GitHub JackieZ123430.
const language = require('./language.js');
const words = require('./locale_zh.js');
const chinese = language === 'zh-CN';
const credit = '汉化：抖音 小白科技杂物社 · BiliBili 小白151 · GitHub JackieZ123430';

// Interpolate after lookup. Repository names, SSIDs, tokens, error payloads and
// other arguments are data; they are never searched or replaced as UI words.
function t(key, ...args) {
    const text = chinese && Object.prototype.hasOwnProperty.call(words, key) ? words[key] : key;
    return String(text).replace(/\{(\d+)\}/g, (match, i) => Number(i) < args.length ? String(args[i]) : match);
}

// Only a layout's static display options use this. Runtime view values and
// actions are merged afterwards, preserving externally supplied text and IDs.
function staticOptions(value) {
    if (Array.isArray(value)) return value.map(staticOptions);
    if (!value || typeof value !== 'object') return value;
    const out = {};
    Object.keys(value).forEach((key) => {
        const v = value[key];
        out[key] = typeof v === 'string' && ['text', 'title', 'label', 'empty', 'labels'].indexOf(key) >= 0
            ? t(v) : v && typeof v === 'object' ? staticOptions(v) : v;
    });
    return out;
}
module.exports = { t, language, chinese, credit, staticOptions };
