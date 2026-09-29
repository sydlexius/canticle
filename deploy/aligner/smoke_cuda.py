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


if not torch.version.cuda:
    fail("torch.version.cuda is unset: this is not a CUDA torch build (%s)" % torch.__version__)
# ctranslate2 4.8.2 is built for CUDA 12 (dlopens libcublas.so.12); a torch
# whose cuBLAS is another major would leave the two unable to share a lib set.
if torch.version.cuda.split(".")[0] != "12":
    fail("torch CUDA %s is not CUDA 12, which ctranslate2 needs" % torch.version.cuda)
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

# ctranslate2 dlopens cuBLAS by soname; this must resolve through the ldconfig
# entry the Dockerfile registers, exactly as ctranslate2 will at runtime. It
# runs in a FRESH interpreter because this process already imported torch,
# whose own RUNPATH loaded cuBLAS: a same-soname dlopen here would succeed
# from that loaded copy and prove nothing about the loader path ctranslate2
# uses when it is imported first.
for soname in ("libcublas.so.12", "libcublasLt.so.12"):
    probe = subprocess.run(
        [sys.executable, "-c", "import ctypes, sys; ctypes.CDLL(sys.argv[1])", soname],
        capture_output=True,
        text=True,
        check=False,
    )
    if probe.returncode != 0:
        failures.append("%s (fresh process): %s" % (soname, probe.stderr.strip().splitlines()[-1:]))

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
    % (torch.__version__, torch.version.cuda, torch.backends.cudnn.version(), len(paths) + 2, count)
)
