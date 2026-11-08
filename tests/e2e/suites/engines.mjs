// Independent schedules and engine selection reuse the shared browser steps.
export function enginesSuite(T) {
  const { page, assert } = T;
  return {
    async enableSecondEngine() {
      // Seeded fleet nodes have no live agent process; set installed engines the way a heartbeat would.
      T.db("update nodes set installed_engines=ARRAY['ookla','librespeed']::text[] where name='VM1'");
      assert.equal(T.db("select array_to_string(installed_engines,',') from nodes where name='VM1'"), 'ookla,librespeed');
      // Reload so the SPA drops its cached GET /api/nodes from before the capability change.
      await page.reload();
      await T.open('#/nodes', 'Nodes');
      await T.settled();
      await T.row('VM1').click();
      await T.tab('Settings');
      await T.settled();
      await page.getByText('LibreSpeed', { exact: true }).scrollIntoViewIfNeeded();
      await page.getByLabel('LibreSpeed interval', { exact: true }).waitFor();
      assert.equal(await page.getByLabel('LibreSpeed interval', { exact: true }).inputValue(), '60');
      await page.getByRole('checkbox', { name: 'LibreSpeed enabled' }).click();
      await T.fill('LibreSpeed interval', '90', page, true);
      await T.press('Save engine schedules');
      await T.toast('Engine schedules saved').waitFor();
      assert.equal(T.db("select s.interval_minutes||':'||s.enabled from engine_schedules s join nodes n on n.id=s.node_id where n.name='VM1' and s.engine='librespeed'"), '90:true');
      await page.reload();
      await T.heading(/VM1/).waitFor();
      await T.tab('Settings');
      await T.settled();
      await page.getByText('LibreSpeed', { exact: true }).scrollIntoViewIfNeeded();
      await page.getByLabel('LibreSpeed interval', { exact: true }).waitFor();
      assert.equal(await page.getByLabel('LibreSpeed interval', { exact: true }).inputValue(), '90');
      assert.equal(await page.getByRole('checkbox', { name: 'LibreSpeed enabled' }).isChecked(), true);
      await page.getByRole('checkbox', { name: 'LibreSpeed enabled' }).click();
      await T.press('Save engine schedules');
      await T.toast('Engine schedules saved').waitFor();
      assert.equal(T.db("select s.enabled from engine_schedules s join nodes n on n.id=s.node_id where n.name='VM1' and s.engine='librespeed'"), 'f');
    },
    async selectEngine() {
      T.db("insert into ip_addresses(ip) values ('203.0.113.71') on conflict do nothing");
      T.db("insert into results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,threads,engine,ip,server,bytes_down,bytes_up,duration_seconds) select '71000000-0000-4000-8000-000000000071',id,now()-interval '1 minute','ok',9,3,4,4,'librespeed','203.0.113.71',jsonb_build_object('name','Libre fixture'),1000,500,1.5 from nodes where name='VM1'");
      for (const [hash, title, path] of [['#/', 'Dashboard', '/api/results'], ['#/history', 'Results', '/api/results'], ['#/stats', 'Statistics', '/api/stats/summary']]) {
        await T.open(hash, title);
        await T.settled();
        const response = page.waitForResponse(r => r.url().includes(path + '?') && new URL(r.url()).searchParams.get('engine') === 'librespeed');
        await T.choose(page.getByLabel('Engine', { exact: true }), 'librespeed');
        const filtered = await response;
        assert.equal(filtered.status(), 200);
        if (path === '/api/results') {
          const body = await filtered.json();
          assert.equal(body.data.length, 1, 'the second engine has one separate result');
          assert.equal(body.data[0].engine, 'librespeed');
          assert.equal(body.data[0].down, 9, 'the result does not contain an Ookla average');
        }
        await T.settled();
        await T.expectNoLoadError();
        assert.equal(await page.getByLabel('Engine', { exact: true }).inputValue(), 'librespeed');
        await T.choose(page.getByLabel('Engine', { exact: true }), '');
        await T.settled();
      }
      T.db("delete from results where id='71000000-0000-4000-8000-000000000071'");
    },
  };
}
