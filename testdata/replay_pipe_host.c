#include <dlfcn.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>
#include <unistd.h>
#include "abi.h"

static int call(void *ctx, const char *method, const uint8_t *data,
                size_t len, cliproxy_buffer *out) { return 1; }
static void release(void *ptr, size_t len) { free(ptr); }

int main(int argc, char **argv) {
    struct sigaction policy;
    if (sigaction(SIGPIPE, NULL, &policy) != 0 || policy.sa_handler != SIG_DFL)
        return 2;
    void *handle = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    if (!handle) { fprintf(stderr, "dlopen: %s\n", dlerror()); return 3; }
    int (*init)(cliproxy_host_api *, cliproxy_plugin_api *) =
        dlsym(handle, "cliproxy_plugin_init");
    if (!init) return 4;
    if (argc > 2) {
        int fds[2];
        if (pipe(fds) != 0) return 5;
        close(fds[0]);
        if (dup2(fds[1], STDOUT_FILENO) < 0) return 6;
        close(fds[1]);
    }
    cliproxy_host_api host = {1, NULL, call, release};
    cliproxy_plugin_api plugin = {0};
    int result = init(&host, &plugin);
    if (result != 0) return 7;
    // Startup output is asynchronous. The subprocess has an outer hard timeout;
    // deterministic writer completion and both inference lanes are tested in Go.
    struct timespec pause = {1, 0};
    nanosleep(&pause, NULL);
    plugin.shutdown();
    fprintf(stderr, "native host survived startup\n");
    return 0;
}
