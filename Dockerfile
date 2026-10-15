# Built in three steps so that the slow part, compiling what weight-tracker depends on,
# is a layer of its own that only a change to Cargo.toml or the lockfile
# undoes: cargo-chef notes what the crate depends on, builds just that, and
# weight-tracker's own source is compiled on top. homelab-rs resolves from its public
# git repository like everything else, so nothing here needs a credential.
FROM --platform=$BUILDPLATFORM lukemathwalker/cargo-chef:latest-rust-1-bookworm AS chef
WORKDIR /src
ARG TARGETARCH
# Built for the cluster's machine by Rust's own cross-compiling, not by
# emulating one. The musl targets carry their own C runtime, so the linker Rust
# ships is all the linking needs.
RUN case "$TARGETARCH" in \
      arm64) echo aarch64-unknown-linux-musl ;; \
      amd64) echo x86_64-unknown-linux-musl ;; \
      *) echo "no target for $TARGETARCH" >&2; exit 1 ;; \
    esac > /target && rustup target add "$(cat /target)"
ENV CARGO_TARGET_AARCH64_UNKNOWN_LINUX_MUSL_LINKER=rust-lld \
    CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER=rust-lld

# The C: jemalloc, which homelab-rs uses as the allocator and for the heap
# profile, and SQLite, which is bundled. Both have to be compiled for the
# target's musl, and zig is a C cross-compiler for every target in one
# download — Debian ships no aarch64-musl gcc. Pinned by checksum, since it is
# a toolchain.
ARG ZIG_VERSION=0.15.2
RUN arch="$(uname -m)" \
    && case "$arch" in \
      x86_64) sum=02aa270f183da276e5b5920b1dac44a63f1a49e55050ebde3aecc9eb82f93239 ;; \
      aarch64) sum=958ed7d1e00d0ea76590d27666efbf7a932281b3d7ba0c6b01b0ff26498f667f ;; \
      *) echo "no zig for $arch" >&2; exit 1 ;; \
    esac \
    && curl -fsSL -o /tmp/zig.tar.xz "https://ziglang.org/download/${ZIG_VERSION}/zig-${arch}-linux-${ZIG_VERSION}.tar.xz" \
    && echo "$sum  /tmp/zig.tar.xz" | sha256sum -c - \
    && mkdir /opt/zig && tar -xJf /tmp/zig.tar.xz -C /opt/zig --strip-components=1 && rm /tmp/zig.tar.xz
# One wrapper per target, which fixes two things zig and jemalloc's build
# disagree on:
#
#   --target=aarch64-unknown-linux-musl is how the cc crate spells the target,
#   and zig rejects it for its own aarch64-linux-musl. Dropped; the wrapper
#   says the target itself.
#
#   -lgcc is how jemalloc's configure looks for _Unwind_Backtrace, the unwinder
#   its heap profiler records stacks with. zig has no libgcc, so the check
#   fails and configure falls back to walking frame pointers instead, which
#   Rust code does not keep: the first sampled allocation in a deep enough
#   stack segfaults the process. zig's libunwind provides the same symbol, so
#   -lgcc becomes -lunwind; at the final link Rust's own libunwind supplies it.
RUN for t in aarch64 x86_64; do \
      { echo '#!/bin/sh'; \
        echo 'for a do shift; case "$a" in --target=*) ;; -lgcc) set -- "$@" -lunwind ;; *) set -- "$@" "$a" ;; esac; done'; \
        echo "exec /opt/zig/zig cc -target $t-linux-musl \"\$@\""; } > /usr/local/bin/$t-musl-cc; \
      printf '#!/bin/sh\nexec /opt/zig/zig ar "$@"\n' > /usr/local/bin/$t-musl-ar; \
      chmod +x /usr/local/bin/$t-musl-cc /usr/local/bin/$t-musl-ar; \
    done
# jemalloc fixes its page size when it is compiled and refuses to start on a
# kernel with larger pages. The Pi's kernel uses 4 KiB (getconf PAGESIZE); a
# 16 KiB kernel, as a Pi 5 boots by default, would need 14 here.
ENV CC_aarch64_unknown_linux_musl=aarch64-musl-cc \
    AR_aarch64_unknown_linux_musl=aarch64-musl-ar \
    CC_x86_64_unknown_linux_musl=x86_64-musl-cc \
    AR_x86_64_unknown_linux_musl=x86_64-musl-ar \
    JEMALLOC_SYS_WITH_LG_PAGE=12

FROM chef AS plan
COPY . .
RUN cargo chef prepare --recipe-path /recipe.json

FROM chef AS build
COPY --from=plan /recipe.json /recipe.json
RUN cargo chef cook --locked --release --target "$(cat /target)" --recipe-path /recipe.json
COPY . .
RUN cargo build --locked --release --target "$(cat /target)" \
    && cp "target/$(cat /target)/release/weight-tracker" /weight-tracker

# scratch: a static binary with its templates and assets compiled in needs
# nothing else. No shell, no libc, nothing to patch. No tzdata either, though
# this app lives on wall-clock time: homelab-rs bundles the zone database, and
# TZ (set in the Deployment, and not cosmetic) chooses the zone. /backup.db and
# the heap profile write temp files, so they need a writable /tmp, which the
# Deployment mounts.
FROM scratch
# The app writes SQLite files here; the Deployment mounts the app's
# PersistentVolumeClaim over it.
WORKDIR /var/lib/weight-tracker
COPY --from=build /weight-tracker /weight-tracker
USER 65532:65532
EXPOSE 8090
ENTRYPOINT ["/weight-tracker"]
CMD ["-addr", ":8090", "-db", "/var/lib/weight-tracker/weight-tracker.db"]
