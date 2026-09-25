import { usePhenixStore } from '@/store.js';
import { minimatch } from 'minimatch';

// results are cached per role object, so a new login (even with a role of
// the same name but different policies) starts with an empty cache
let cache = new Map();
let cacheRole = null;
// should match role.go#Allowed (with added caching)
export function roleAllowed(resource, verb, ...names) {
  let phenixStore = usePhenixStore();
  let role = phenixStore.role;
  if (role === null) {
    return false;
  }

  if (role !== cacheRole) {
    cache = new Map();
    cacheRole = role;
  }

  let k = [resource, verb, names].join('$');
  if (cache.has(k)) {
    return cache.get(k);
  }

  for (const p of role.policies) {
    for (const r of p.resources) {
      if (minimatch(resource, r)) {
        for (const v of p.verbs) {
          if (v == '*' || v == verb) {
            if (names.length == 0) {
              cache.set(k, true);
              return true;
            }
            for (const name of names) {
              if (name && resourceNameAllowed(p, name)) {
                cache.set(k, true);
                return true;
              }
            }
          }
        }
      }
    }
  }
  cache.set(k, false);
  return false;
}

// should match policy.go#resourceNameAllowed
let resourceNameAllowed = (policy, name) => {
  var allowed = false;
  for (const n of policy.resourceNames) {
    let negate = n.startsWith('!');
    var n2 = n.replace('!', '');

    if (name.includes('/') && !n2.includes('/')) {
      n2 = '*/' + n2;
    }

    if (minimatch(name, n2)) {
      if (negate) {
        return false;
      }
      allowed = true;
    }
  }
  return allowed;
};
