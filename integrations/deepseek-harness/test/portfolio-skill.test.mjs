import assert from 'node:assert/strict';
import test from 'node:test';
import { inject, apply } from '../src/plugin.mjs';
import { portfolioSkill } from '../src/portfolio-skill.mjs';
import { portfolioOutput, portfolioParameters } from '../src/schemas.mjs';

function pluginContext() {
  const tools = new Map();
  const skills = new Map();
  const cleanups = [];
  const ctx = {
    tools: { register: tool => tools.set(tool.name, tool) },
    skills: {
      register(skill) {
        skills.set(skill.name, skill);
        return () => skills.delete(skill.name);
      },
    },
    effect(setup) { cleanups.push(setup()); },
  };
  return {
    ctx, tools, skills,
    dispose() { for (const cleanup of cleanups.splice(0).reverse()) cleanup(); },
  };
}

test('portfolio Tool exposes a closed credential contract and bounded account count', () => {
  assert.deepEqual(portfolioParameters.required, ['accounts']);
  assert.equal(portfolioParameters.additionalProperties, false);
  assert.equal(portfolioParameters.properties.accounts.minItems, 2);
  assert.equal(portfolioParameters.properties.accounts.maxItems, 10);
  assert.deepEqual(portfolioParameters.properties.accounts.items.required, ['username', 'password']);
  assert.equal(portfolioParameters.properties.accounts.items.additionalProperties, false);
  assert.equal(portfolioOutput.schema.oneOf.length, 2);
  assert.equal(portfolioOutput.schema.oneOf.every(branch => branch.additionalProperties === false), true);
});

test('plugin publishes and disposes one runtime portfolio Skill beside seven Tools', () => {
  const runtime = pluginContext();
  apply(runtime.ctx, { apiBaseUrl: 'http://127.0.0.1:18080' });

  assert.deepEqual(inject, ['tools', 'skills']);
  assert.equal(runtime.tools.size, 7);
  assert.ok(runtime.tools.has('auction_get_multi_account_report'));
  assert.deepEqual(runtime.tools.get('auction_get_multi_account_report').parameters, portfolioParameters);
  assert.equal(runtime.skills.size, 1);
  assert.equal(runtime.skills.get('analyzing-auction-portfolio'), portfolioSkill);
  assert.equal(portfolioSkill.source, 'runtime');

  runtime.dispose();
  assert.equal(runtime.skills.size, 0);
});

test('plugin fails explicitly when the Harness skills service is absent', () => {
  assert.throws(() => apply({ tools: { register() {} }, effect() {} }), /skills service/);
});
