'use strict';
/* Runs installs through the copy built from fixtures/main_queue.js (argv[2]):
 * six of BMW's apps that fail with a network error (60 ms a try, retry
 * delay from 40 ms, doubling), then ours, a second of ours and one app not
 * listed, each 3 ms. Prints the order the apps ran in and the log. */
const built = require(process.argv[2]);
const bmw = ['bmw_1', 'bmw_2', 'bmw_3', 'bmw_4', 'bmw_5', 'bmw_6'];
const ran = [], done = {}, t0 = Date.now();
const ctl = new built.AppManagementController((app) => new Promise((res, rej) => {
    ran.push(app);
    setTimeout(() => (bmw.indexOf(app) >= 0 ? rej(new Error('NETWORK_ERROR')) : res()), bmw.indexOf(app) >= 0 ? 60 : 3);
}), 40);
bmw.forEach((a) => ctl.install(a).catch(() => {}));
setTimeout(() => {
    ['our_app', 'their_app', 'other_app'].forEach((a) => ctl.install(a).then(() => { done[a] = Date.now() - t0; }));
}, 100);
setTimeout(() => {
    console.log(JSON.stringify({ ran: ran.slice(0, 12), done, log: (global.__fixtureLog || []).filter((l) => /^mgu-deploy/.test(l)) }));
    process.exit(0);
}, 1500);
