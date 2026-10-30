'use strict';
// RHMI Text is ASCII on some units. Unicode goes through UTF-8 StringBundles.
const COUNT = 256;
const slot = (n) => 'SID_STORE_DYNAMIC_' + ('0000' + n).slice(-4);
class HmiText {
    constructor() { this.slots = new Map(); this.pending = new Map(); }
    set(component, name, row, cell, text) {
        if (!/[^\x20-\x7e]/.test(text)) return component.setValue(row, cell, text, 'Text');
        const key = JSON.stringify([name, row, cell]);
        if (!this.slots.has(key)) {
            if (this.slots.size >= COUNT) return Promise.reject(new Error('Unicode text resource pool exhausted'));
            this.slots.set(key, slot(this.slots.size));
        }
        const id = this.slots.get(key), values = { [id]: text };
        const next = (this.pending.get(key) || Promise.resolve()).catch(() => {})
            .then(() => component.getStringBundle().setStrings(values))
            .then(() => component.setValue(row, cell, id, 'StringId'));
        this.pending.set(key, next);
        return next;
    }
}
module.exports = { HmiText, COUNT, slot };
