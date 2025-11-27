let lockCount = 0;

function applyLock() {
  document.body.style.overflow = lockCount > 0 ? 'hidden' : '';
}

export function scrollLock(node: HTMLElement, enabled: boolean) {
  let locked = Boolean(enabled);

  if (locked) {
    lockCount += 1;
    applyLock();
  }

  return {
    update(nextEnabled: boolean) {
      const nextLocked = Boolean(nextEnabled);
      if (nextLocked === locked) return;
      if (nextLocked) {
        lockCount += 1;
      } else if (lockCount > 0) {
        lockCount -= 1;
      }
      locked = nextLocked;
      applyLock();
    },
    destroy() {
      if (locked && lockCount > 0) {
        lockCount -= 1;
        applyLock();
      }
    }
  };
}
