-- wrk script for bench/phase6/race-profile.sh: every request goes to
-- many.test with a random X-Forwarded-For, which the proxy believes because
-- 127.0.0.1 is a trusted proxy in that run. Each request is then, most
-- likely, a client the rate-limit table has not seen, so it inserts, evicts
-- and sweeps.
local thread_id = 0
function setup(thread)
  thread_id = thread_id + 1
  thread:set("seed", thread_id)
end

function init(args)
  math.randomseed(os.time() + seed * 7919)
end

function request()
  local xff = string.format("%d.%d.%d.%d", math.random(11, 223), math.random(0, 255),
    math.random(0, 255), math.random(1, 254))
  return wrk.format("GET", "/", { ["Host"] = "many.test", ["X-Forwarded-For"] = xff })
end
