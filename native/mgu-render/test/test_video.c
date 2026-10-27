/*
 * Video frame files (gfx.c) on Mesa's surfaceless EGL: a file shorter than its header says is not looked for again
 * until its next frame, and videos count towards the picture limits (32 MB an app, 96 MB all apps).
 *   EGL_PLATFORM=surfaceless build/host/test_video
 */
#include <EGL/egl.h>
#include <GLES2/gl2.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include "gfx.h"
#include "proto.h"

static int fails;
#define CHECK(cond, what) do { if (cond) printf("ok   %s\n", what); else { printf("FAIL %s\n", what); fails++; } } while (0)

static const char *PATH = "/dev/shm/mgu-video-test-video";

/* the header (seq) and `frames` whole frames of w x h */
static void write_file(uint32_t w, uint32_t h, uint32_t seq, int frames) {
    struct mgu_video_head hd = { MGU_VIDEO_MAGIC, 1, w, h, seq, 0, 1, 0, 0, { 0 } };
    int fd = open(PATH, O_WRONLY | O_CREAT | O_TRUNC, 0600);
    if (fd < 0 || write(fd, &hd, sizeof hd) != sizeof hd) { perror(PATH); exit(1); }
    size_t fsz = (size_t)w * h * 3 / 2;
    uint8_t *fr = calloc(1, fsz);
    for (int k = 0; k < frames; k++) if (write(fd, fr, fsz) != (ssize_t)fsz) { perror(PATH); exit(1); }
    free(fr); close(fd);
}

static void draw(gfx_client *c) {
    gfx_xf xf = { 1, 0, 0 };
    gfx_draw(c, &xf, 16, 16, NULL, 0, 1);
}

static gfx_video *video(gfx_client *c, uint32_t id) {
    for (gfx_video *v = c->videos; v; v = v->next) if (v->id == id) return v;
    return NULL;
}

static int shown(gfx_client *c) {
    int n = 0;
    for (gfx_video *v = c->videos; v; v = v->next) n += v->w != 0;
    return n;
}

static uint32_t first_refused(gfx_client *c) {
    for (gfx_video *v = c->videos; v; v = v->next) if (!v->w) return v->id;
    return 0;
}

int main(void) {
    EGLDisplay d = eglGetDisplay(EGL_DEFAULT_DISPLAY);
    EGLint ca[] = { EGL_SURFACE_TYPE, EGL_PBUFFER_BIT, EGL_RENDERABLE_TYPE, EGL_OPENGL_ES2_BIT, EGL_NONE }, n;
    EGLint xa[] = { EGL_CONTEXT_CLIENT_VERSION, 2, EGL_NONE }, pa[] = { EGL_WIDTH, 16, EGL_HEIGHT, 16, EGL_NONE };
    EGLConfig cfg;
    if (!eglInitialize(d, NULL, NULL) || !eglChooseConfig(d, ca, &cfg, 1, &n) || n < 1) { puts("FAIL no EGL"); return 1; }
    eglBindAPI(EGL_OPENGL_ES_API);
    EGLContext ctx = eglCreateContext(d, cfg, EGL_NO_CONTEXT, xa);
    EGLSurface pb = eglCreatePbufferSurface(d, cfg, pa);
    if (!eglMakeCurrent(d, pb, pb, ctx) || gfx_init() < 0) { puts("FAIL no GL context"); return 1; }
    uint32_t me = (uint32_t)getuid();

    /* short read: the header promises a 64x32 frame the file does not hold */
    gfx_client c; memset(&c, 0, sizeof c); c.name = "test";
    write_file(64, 32, 1, 0);
    CHECK(gfx_video_def(&c, 1, PATH, me) == 0, "the app's own frame file is opened");
    CHECK(gfx_video_pending(&c), "a new frame is pending");
    draw(&c);
    CHECK(!gfx_video_pending(&c), "a frame that cannot be read is not pending again");
    CHECK(video(&c, 1)->w == 0, "nothing shown for it");
    write_file(64, 32, 2, 1);
    CHECK(gfx_video_pending(&c), "the next frame is pending");
    draw(&c);
    CHECK(!gfx_video_pending(&c) && video(&c, 1)->w == 64, "the next frame is shown");
    gfx_client_free(&c);
    CHECK(c.image_bytes == 0, "dropping the video frees its bytes");

    /* owner */
    if (me != 0) {
        CHECK(gfx_video_def(&c, 1, PATH, me + 1) < 0 && strstr(gfx_error(), "is not the app's"), "another uid's frame file is refused");
        CHECK(gfx_video_def(&c, 1, PATH, 0) == 0, "root (bring-up tools) opens any");
        gfx_client_free(&c);
    }

    /* limits: a 2048x2048 video counts 12 MiB (6 MiB in memory, 6 on the GPU) */
    write_file(2048, 2048, 1, 1);
    size_t one = (size_t)2048 * 2048 * 3;
    for (uint32_t id = 1; id <= 3; id++) gfx_video_def(&c, id, PATH, me);
    draw(&c);
    uint32_t refused = first_refused(&c);
    CHECK(shown(&c) == 2 && refused, "an app's third 2048x2048 video is past 32 MB");
    CHECK(c.image_bytes == 2 * one, "two counted");
    CHECK(!gfx_video_pending(&c), "the refused frame is not pending");
    gfx_video_drop(&c, refused == 1 ? 2 : 1);
    draw(&c);
    CHECK(video(&c, refused)->w == 2048 && c.image_bytes == 2 * one, "shown once another is dropped");
    gfx_client_free(&c);

    gfx_client k[5]; memset(k, 0, sizeof k);
    int all = 0;
    for (int i = 0; i < 5; i++) {
        k[i].name = "test";
        for (uint32_t id = 1; id <= 2; id++) gfx_video_def(&k[i], id, PATH, me);
        draw(&k[i]);
        all += shown(&k[i]);
    }
    CHECK(all == 8, "all apps show eight 2048x2048 videos, 96 MB, and no more");
    for (int i = 0; i < 5; i++) gfx_client_free(&k[i]);

    unlink(PATH);
    printf("%s\n", fails ? "FAILURES" : "video: all ok");
    return fails != 0;
}
