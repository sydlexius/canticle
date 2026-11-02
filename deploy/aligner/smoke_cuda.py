"""Build-time inverted check for the CUDA image variant (#1013).

There is no GPU at image build time, so this cannot prove the model runs on
one. It proves the failure class that matters instead: a CUDA library that
torch or ctranslate2 needs is missing or unloadable, which would otherwise
surface as a segfault or `Could not load library ...` on the first /align.

Run by the Dockerfile only when VARIANT=cuda; `python smoke_cuda.py` also
works against a built image. Exits non-zero (failing the build) on any miss.
"""

import ctypes
import glob
import os
import subprocess
import sys
import sysconfig

import torch


def fail(msg: str) -> None:
    print("FATAL: " + msg, file=sys.stderr)
    sys.exit(1)


# torch's CUDA major (the lock's +cu132 build). ctranslate2 is a DIFFERENT
# major: 4.8.4 is a CUDA 12 build that dlopens libcublas.so.12, which the
# image carries separately (nvidia-cublas-cu12, requirements-cuda.in) and
# which is checked by soname below. Changing torch's CUDA line is a deliberate
# edit here, the lock, the Dockerfile and the README's host-driver floor.
TORCH_CUDA_MAJOR = "13"
# What ctranslate2 dlopens by soname (libcublas.so.12 NEEDs libcublasLt.so.12),
# and the directory the Dockerfile registers with ldconfig for it.
CT2_CUBLAS = ("libcublas.so.12", "libcublasLt.so.12")
CT2_CUBLAS_DIR = os.path.join("nvidia", "cublas", "lib")

if not torch.version.cuda:
    fail("torch.version.cuda is unset: this is not a CUDA torch build (%s)" % torch.__version__)
if torch.version.cuda.split(".")[0] != TORCH_CUDA_MAJOR:
    fail("torch CUDA %s is not CUDA %s, the line the CUDA lock pins" % (torch.version.cuda, TORCH_CUDA_MAJOR))
if not torch.backends.cudnn.version():
    fail("torch reports no cuDNN")

# A driver-provided library is injected by the NVIDIA container toolkit at
# container start, so its absence at build time is expected and not a defect.
DRIVER_LIBS = ("libcuda.so.1",)

# Optional cluster/storage plugins shipped inside nvidia-nvshmem / nvidia-cufile
# (MPI, PMIx, OpenSHMEM, libfabric, UCX and RDMA transports). They dlopen only
# when a multi-node run selects them and depend on host libraries a slim image
# does not carry; single-GPU inference never loads them.
OPTIONAL_PLUGINS = ("nvshmem_bootstrap_", "nvshmem_transport_", "libcufile_rdma")


def load(path: str, failures: list) -> None:
    if os.path.basename(path).startswith(OPTIONAL_PLUGINS):
        return
    try:
        ctypes.CDLL(path)
    except OSError as exc:
        if any(d in str(exc) for d in DRIVER_LIBS):
            print("skip (needs driver): %s" % path)
            return
        failures.append("%s: %s" % (path, exc))


site = sysconfig.get_paths()["purelib"]
failures: list = []

# ctranslate2 dlopens cuBLAS 12 by soname; it must resolve through the
# ldconfig entry the Dockerfile registers, exactly as ctranslate2 will at
# runtime, and from the CUDA 12 wheel's directory. Each probe runs in a FRESH
# interpreter so nothing this process already loaded can satisfy the dlopen,
# and reports the file the loader actually mapped (/proc/self/maps).
#   alone: ctranslate2 imported first, before torch. This is the probe that
#     proves the Dockerfile's ldconfig entry (it fails without it).
#   after torch: the real /align process shape. torch has already mapped its
#     own CUDA 13 cuBLAS (libcublas.so.13 via its DT_RPATH), and the CUDA 12
#     copy must still load beside it rather than be satisfied by, or clash
#     with, the CUDA 13 one. Note torch 2.15.0's _preload_cuda_deps globs
#     nvidia/cublas/lib BEFORE nvidia/cu13/lib, so `import torch` itself
#     ctypes-loads (RTLD_LOCAL) the CUDA 12 copy here: this probe passes even
#     without ldconfig, which is why the "alone" probe exists. That preload is
#     harmless (measured with LD_DEBUG=bindings: every torch cuBLAS symbol binds
#     to libcublas.so.13, none to .12; the sonames differ and RTLD_LOCAL keeps
#     the .12 symbols out of global scope).
PROBE = r"""
import ctypes, sys
if sys.argv[1] == "torch":
    import torch
ctypes.CDLL(sys.argv[2])
maps = open("/proc/self/maps").read().split()
print("\n".join(sorted({p for p in maps if p.startswith("/") and "/libcublas" in p})))
"""
cublas12_dir = os.path.join(site, CT2_CUBLAS_DIR)
for mode in ("alone", "torch"):
    for soname in CT2_CUBLAS:
        probe = subprocess.run(
            [sys.executable, "-c", PROBE, mode, soname],
            capture_output=True,
            text=True,
            check=False,
        )
        label = "%s (fresh process, %s)" % (soname, "after import torch" if mode == "torch" else "alone")
        if probe.returncode != 0:
            failures.append("%s: %s" % (label, probe.stderr.strip().splitlines()[-1:]))
            continue
        mapped = probe.stdout.split()
        want = os.path.join(cublas12_dir, soname)
        if not any(os.path.realpath(m) == os.path.realpath(want) for m in mapped):
            failures.append("%s: loaded, but not from %s; mapped cuBLAS: %s" % (label, want, mapped))
            continue
        print("%s -> %s" % (label, ", ".join(mapped)))
        if mode == "torch" and not any(".so.%s" % TORCH_CUDA_MAJOR in os.path.basename(m) for m in mapped):
            failures.append("%s: torch mapped no CUDA %s cuBLAS beside it: %s" % (label, TORCH_CUDA_MAJOR, mapped))

# Every shared library torch ships plus every pip-provided NVIDIA library: a
# wheel that installed but cannot load (missing transitive dependency, wrong
# glibc) fails here. Sorted so the log is stable.
paths = sorted(
    glob.glob(os.path.join(site, "torch", "lib", "*.so*")) + glob.glob(os.path.join(site, "nvidia", "*", "lib", "*.so*"))
)
if not paths:
    fail("found no torch/nvidia shared libraries under %s" % site)
for path in paths:
    load(path, failures)

import ctranslate2  # noqa: E402

# Reports 0 with no driver; it raises only if the CUDA runtime itself is broken.
count = ctranslate2.get_cuda_device_count()

if failures:
    fail("%d library(ies) failed to load:\n  %s" % (len(failures), "\n  ".join(failures)))
print(
    "cuda smoke ok: torch %s (CUDA %s, cuDNN %s), %d libraries loaded, ctranslate2 sees %d device(s)"
    % (torch.__version__, torch.version.cuda, torch.backends.cudnn.version(), len(paths), count)
)
