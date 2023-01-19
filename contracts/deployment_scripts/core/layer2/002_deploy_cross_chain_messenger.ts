import {HardhatRuntimeEnvironment} from 'hardhat/types';
import {DeployFunction} from 'hardhat-deploy/types';
import { ethers } from 'hardhat';

const func: DeployFunction = async function (hre: HardhatRuntimeEnvironment) {
    const { 
        deployments, 
        getNamedAccounts
    } = hre;

    // Get the prefunded L2 deployer account to use for deploying.
    const {deployer} = await getNamedAccounts();

    // TODO: Remove hardcoded L2 message bus address when properly exposed.
    const busAddress = hre.ethers.utils.getAddress("0x526c84529b2b8c11f57d93d3f5537aca3aecef9b")

    console.log(`Beginning deploy of cross chain messenger`);

    // Deploy the L2 Cross chain messenger and use the L2 bus for validation
    await deployments.deploy('CrossChainMessenger', {
    from: deployer,
        args: [ busAddress ],
        log: true,
    });
};

export default func;
func.tags = ['CrossChainMessenger', 'CrossChainMessenger_deploy'];