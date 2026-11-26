"""Replay a live follow-up through public HTTP and verify its one Worker owner."""
import hashlib
import json
import time
import uuid


def verify_active_replay(client, kube, turn, message, checkpoint):
    assert str(uuid.UUID(turn)) == turn
    assert isinstance(message, str) and message
    route = client.route + "/turns/" + turn

    def read(path):
        return json.loads(client.request("GET", path))

    original = read(route)
    assert original["turn_id"] == turn and original["state"] == "running"
    assert original["parent"] == checkpoint and not original.get("assets")
    assert original["spec"]["instruction"] == message
    assert read(client.route + "/checkpoint") == checkpoint

    def owner():
        jobs = json.loads(kube("get", "jobs", "-l", "agentmoor.io/turn=" + turn, "-o", "json"))["items"]
        job, = jobs
        pods = json.loads(kube("get", "pods", "-l", "job-name=" + job["metadata"]["name"], "-o", "json"))["items"]
        pod, = pods
        assert pod["status"]["phase"] == "Running"
        worker, = [row for row in pod["status"]["containerStatuses"] if row["name"] == "worker"]
        assert "running" in worker["state"] and type(worker["restartCount"]) is int and worker["restartCount"] == 0
        identity = {"job_uid": job["metadata"]["uid"], "pod_uid": pod["metadata"]["uid"],
                    "worker_container_id": worker["containerID"], "image_id": worker["imageID"]}
        assert all(isinstance(value, str) and value for value in identity.values())
        return identity

    identity = owner()

    def unchanged():
        assert read(route) == original, "active replay changed the accepted running record"
        assert read(client.route + "/checkpoint") == checkpoint, "active replay changed the successful checkpoint"
        assert owner() == identity, "active replay changed or duplicated the Worker owner"

    question = {"session_id": checkpoint["session_id"], "message": message}
    started = time.monotonic()
    for _ in range(2):
        assert json.loads(client.request("PUT", route, question)) == original
        unchanged()
    # The dispatcher reconciles once per second. Observe two more cycles so a
    # delayed duplicate Job cannot pass an immediate post-request snapshot.
    for _ in range(2):
        time.sleep(1)
        unchanged()
    proof = {"turn_id": turn, "state": "running", "same_uuid_replays": 2,
             "record_unchanged": True, "checkpoint_unchanged": True, "assets_empty": True,
             "job_count": 1, "pod_count": 1, "worker_restart_count": 0,
             "observation_seconds": round(time.monotonic() - started, 3),
             "checkpoint_sha256": hashlib.sha256(json.dumps(checkpoint, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
             **identity}
    print("ACTIVE_REPLAY_RESULT " + json.dumps(proof, sort_keys=True), flush=True)
    return proof
