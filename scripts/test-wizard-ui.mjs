// Own headless browser only; never attaches to an existing browser or app.
import { chromium, expect } from '../client/node_modules/@playwright/test/index.mjs';
import { mkdir } from 'node:fs/promises';
const base=process.env.HAUNT_WIZARD_URL;
if(!base||!process.env.HAUNT_WIZARD_TOKEN||!process.env.HAUNT_WIZARD_PASSWORD)throw Error('Missing isolated wizard fixture');
const browser=await chromium.launch({headless:true});
try{
 const context=await browser.newContext();
 const page=await context.newPage();
 const errors=[];page.on('pageerror',error=>errors.push(error.message));
 await page.goto(base+'/');
 await expect(page.getByRole('heading',{name:'Welcome to Haunt'})).toBeVisible();
 await page.locator('#setup-code').fill(process.env.HAUNT_WIZARD_TOKEN);
 await page.getByRole('button',{name:'Open setup'}).click();
 await expect(page.getByRole('heading',{name:'Make it yours'})).toBeVisible();
 await page.locator('[name="HAUNT_NAME"]').fill('Wizard community');
 const database=page.locator('[name="HAUNT_DATABASE_URL"]');
 if(await database.isVisible() && await database.isEnabled())await database.fill(process.env.HAUNT_WIZARD_DATABASE_URL);
 await page.getByRole('button',{name:'Continue',exact:true}).click();
 await expect(page.getByRole('heading',{name:'A comfortable starting point'})).toBeVisible();
 await page.locator('[name="HAUNT_TEXT_RETENTION_DAYS"]').fill('44');
 const directory=process.env.HAUNT_WIZARD_SCREENSHOTS;
 if(directory){await mkdir(directory,{recursive:true});await page.screenshot({path:directory+'/preferences.png',fullPage:true});}
 await page.getByRole('button',{name:'Continue',exact:true}).click();
 await expect(page.getByRole('heading',{name:'Your owner account'})).toBeVisible();
 await page.locator('#password').fill(process.env.HAUNT_WIZARD_PASSWORD);
 await page.locator('#confirm-password').fill('mismatched-test-password');
 await page.getByRole('button',{name:'Create my server'}).click();
 await expect(page.getByRole('alert')).toHaveText('The passwords do not match.');
 await page.locator('#confirm-password').fill(process.env.HAUNT_WIZARD_PASSWORD);
 if(directory)await page.screenshot({path:directory+'/owner.png',fullPage:true});
 await page.getByRole('button',{name:'Create my server'}).click();
 await expect(page.getByRole('heading',{name:'Your server is ready'})).toBeVisible({timeout:60000});
 if(directory)await page.screenshot({path:directory+'/complete.png',fullPage:true});
 const instance=await page.request.get(base+'/api/v1/instance');
 if(!instance.ok())throw Error('Normal API did not replace setup on its original port');
 const data=await instance.json();
 if(data.name!=='Wizard community'||data.limits.text_retention_days!==44)throw Error('Selected settings were not applied');
 const login=await page.request.post(base+'/api/v1/auth/login',{data:{username:'owner',password:process.env.HAUNT_WIZARD_PASSWORD,device_name:'rendered wizard verification'}});
 if(!login.ok())throw Error('Owner-chosen password did not sign in');
 await page.getByRole('link',{name:'Open Haunt'}).click();
 await expect(page.locator('#wizard')).toHaveCount(0);
 if(errors.length)throw Error(errors.join('\n'));
 console.log('PASS: rendered wizard, password mismatch, chosen owner login, actual settings, same-port client handover');
 await context.close();
}finally{await browser.close();}
