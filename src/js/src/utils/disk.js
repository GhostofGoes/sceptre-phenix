export function getDiskLabel(disk) {
  return disk.displayName || disk.name || disk.fullPath;
}
